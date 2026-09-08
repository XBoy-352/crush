// Package childjobs owns background subagent and workflow jobs.
//
// It is a separate package so internal/agent/tools can import it without
// creating an import cycle with internal/agent. It must not import
// internal/agent.
package childjobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Kind string

const (
	KindAgent          Kind = "agent"
	KindFetch          Kind = "fetch"
	KindWorkflow       Kind = "workflow"
	KindWorkflowWorker Kind = "workflow-worker"
)

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusError   Status = "error"
	StatusKilled  Status = "killed"
)

const (
	// DefaultMaxConcurrentPerSession is the per-parent-session semaphore
	// capacity. Starts beyond the limit queue instead of failing.
	DefaultMaxConcurrentPerSession = 5

	// HardMaxConcurrentPerSession is the runaway backstop on total jobs
	// (queued plus running) per parent session.
	HardMaxConcurrentPerSession = 20
)

// ErrNotFound is returned by Kill when no killable job with the given ID
// exists, mirroring shell.ErrBackgroundShellNotFound.
var ErrNotFound = errors.New("child job not found")

type Job struct {
	ID              string
	Kind            Kind
	ParentSessionID string
	ChildSessionID  string // empty for a workflow root job
	ToolCallID      string
	Title           string
	StartedAt       time.Time

	mu     sync.RWMutex
	status Status
	result string // full final text; set on done
	err    string // set on error/killed

	cancel      context.CancelFunc
	runFn       func(ctx context.Context) (string, error)
	onDoneFn    func(j *Job)
	tookSlot    bool
	settledOnce sync.Once
}

// Status returns the job's current status.
func (j *Job) Status() Status {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.status
}

// Result returns the job's final text; only meaningful when Done.
func (j *Job) Result() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.result
}

// Err returns the job's error text; only meaningful when Done.
func (j *Job) Err() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.err
}

// Snapshot returns a consistent copy of the job's mutable fields.
func (j *Job) Snapshot() (Status, string, string) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.status, j.result, j.err
}

func (j *Job) setStatus(s Status, result, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = s
	j.result = result
	j.err = errMsg
}

// Done reports whether the job reached a terminal state.
func (j *Job) Done() bool {
	switch j.Status() {
	case StatusDone, StatusError, StatusKilled:
		return true
	default:
		return false
	}
}

func jobIDPrefix(kind Kind) string {
	switch kind {
	case KindAgent:
		return "agent"
	case KindFetch:
		return "fetch"
	case KindWorkflow, KindWorkflowWorker:
		return "wf"
	default:
		return "job"
	}
}

type Registry struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string // insertion order, for oldest-first listing
	sems    map[string]chan struct{}
	counter atomic.Uint64
}

var (
	registry     *Registry
	registryOnce sync.Once
)

// GetRegistry returns the process-global child job registry, mirroring
// shell.GetBackgroundShellManager.
func GetRegistry() *Registry {
	registryOnce.Do(func() {
		registry = &Registry{
			jobs: make(map[string]*Job),
			sems: make(map[string]chan struct{}),
		}
	})
	return registry
}

// Start registers j and launches run either immediately (a semaphore slot
// is free) or once one frees. run receives the job's detached context:
// derived via context.WithoutCancel from parentCtx, so the job survives the
// tool call returning and a parent-session cancel; the registry's cancel is
// the only kill switch. onDone is called exactly once, after run returns,
// with j.Status/Result/Err already set.
func (r *Registry) Start(parentCtx context.Context, j *Job, run func(ctx context.Context) (string, error), onDone func(j *Job)) error {
	if err := r.register(j, run, onDone, true); err != nil {
		return err
	}
	r.maybeLaunch(j)
	return nil
}

// StartDetached registers j and launches run immediately, bypassing the
// per-session semaphore. Used for workflow workers, whose concurrency the
// workflow engine's own MaxConcurrent semaphore already bounds.
func (r *Registry) StartDetached(parentCtx context.Context, j *Job, run func(ctx context.Context) (string, error), onDone func(j *Job)) error {
	if err := r.register(j, run, onDone, false); err != nil {
		return err
	}
	r.mu.Lock()
	r.launch(j)
	r.mu.Unlock()
	return nil
}

// register assigns the ID and stores the job in queued state.
func (r *Registry) register(j *Job, run func(ctx context.Context) (string, error), onDone func(j *Job), takesSlot bool) error {
	if run == nil {
		return errors.New("child job requires a run function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	total := 0
	for _, other := range r.jobs {
		if other.ParentSessionID == j.ParentSessionID && !other.Done() {
			total++
		}
	}
	if total >= HardMaxConcurrentPerSession {
		return fmt.Errorf("session %s reached the hard limit of %d concurrent child jobs", j.ParentSessionID, HardMaxConcurrentPerSession)
	}

	j.ID = fmt.Sprintf("%s-%03d", jobIDPrefix(j.Kind), r.counter.Add(1))
	j.setStatus(StatusQueued, "", "")
	j.StartedAt = time.Now()
	j.runFn = run
	j.onDoneFn = onDone
	j.tookSlot = takesSlot
	r.jobs[j.ID] = j
	r.order = append(r.order, j.ID)
	if _, ok := r.sems[j.ParentSessionID]; !ok {
		r.sems[j.ParentSessionID] = make(chan struct{}, DefaultMaxConcurrentPerSession)
	}
	return nil
}

// maybeLaunch launches j if a semaphore slot is free; otherwise it stays
// queued until a running job for the same session hands its slot over.
func (r *Registry) maybeLaunch(j *Job) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !j.tookSlot {
		r.launch(j)
		return
	}
	sem := r.sems[j.ParentSessionID]
	select {
	case sem <- struct{}{}:
		r.launch(j)
	default:
		// Stays queued.
	}
}

// launch marks j running and starts its goroutine.
func (r *Registry) launch(j *Job) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(context.Background()))
	j.cancel = cancel
	j.setStatus(StatusRunning, "", "")

	go func() {
		result, err := j.runFn(ctx)
		j.settledOnce.Do(func() { r.settle(j, ctx, result, err) })
	}()
}

// settle records the terminal state and fires onDone exactly once. Called
// from the job goroutine, or synchronously from Kill for a queued job.
func (r *Registry) settle(j *Job, ctx context.Context, result string, err error) {
	switch {
	case ctx.Err() != nil:
		j.setStatus(StatusKilled, "", "killed")
	case err != nil:
		j.setStatus(StatusError, "", err.Error())
	default:
		j.setStatus(StatusDone, result, "")
	}

	if j.onDoneFn != nil {
		j.onDoneFn(j)
	}

	if j.tookSlot {
		r.handOffSlot(j.ParentSessionID)
	}
}

// handOffSlot releases one semaphore slot for the session and launches the
// oldest queued job for that session, if any.
func (r *Registry) handOffSlot(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sem, ok := r.sems[sessionID]
	if !ok {
		return
	}
	<-sem

	for _, id := range r.order {
		q := r.jobs[id]
		if q.ParentSessionID != sessionID || q.Status() != StatusQueued {
			continue
		}
		select {
		case sem <- struct{}{}:
			r.launch(q)
		default:
		}
		break
	}
}

// Kill stops a job. A queued job is marked killed and never starts; a
// running job is cancelled and its onDone still fires with status killed.
// Killing a KindWorkflow root also kills every worker whose ToolCallID has
// the root's ToolCallID as a prefix. Returns ErrNotFound for unknown or
// already-terminal jobs.
func (r *Registry) Kill(id string) error {
	r.mu.Lock()
	j, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if j.Done() {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s (already finished)", ErrNotFound, id)
	}

	// Workflow root kill cascades to workers by ToolCallID prefix.
	targets := []*Job{j}
	if j.Kind == KindWorkflow && j.ToolCallID != "" {
		for _, other := range r.jobs {
			if other != j && other.Kind == KindWorkflowWorker && !other.Done() &&
				strings.HasPrefix(other.ToolCallID, j.ToolCallID) {
				targets = append(targets, other)
			}
		}
	}

	var queued []*Job
	for _, job := range targets {
		switch status := job.Status(); {
		case status == StatusQueued:
			job.setStatus(StatusKilled, "", "killed before start")
			queued = append(queued, job)
		case job.cancel != nil:
			job.cancel()
		}
	}
	r.mu.Unlock()

	// Settle queued jobs synchronously so a killed-queued job never starts.
	for _, job := range queued {
		job.settledOnce.Do(func() {
			if job.onDoneFn != nil {
				job.onDoneFn(job)
			}
		})
	}
	return nil
}

// Get returns the job with the given ID.
func (r *Registry) Get(id string) (*Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

// List returns all jobs, oldest first.
func (r *Registry) List() []*Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Job, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.jobs[id])
	}
	return out
}

// ListByParent returns the parent session's jobs, oldest first.
func (r *Registry) ListByParent(parentSessionID string) []*Job {
	all := r.List()
	return slices.DeleteFunc(all, func(j *Job) bool { return j.ParentSessionID != parentSessionID })
}
