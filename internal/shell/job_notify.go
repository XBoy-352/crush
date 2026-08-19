package shell

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// JobEventType discriminates background-job lifecycle transitions.
type JobEventType string

const (
	// JobStarted fires when a shell is handed back as a job, not when it
	// starts running.
	JobStarted JobEventType = "started"
	// JobCompleted fires when a backgrounded shell exits.
	JobCompleted JobEventType = "completed"
	// JobRemoved fires when a job is killed or reaped.
	JobRemoved JobEventType = "removed"
)

// JobEvent describes one lifecycle transition. It says only that a job
// changed and whose it is: subscribers re-read whatever they render, and
// putting the details on the wire would mean shipping them per event to
// every client for nobody to read.
type JobEvent struct {
	Type      JobEventType
	ShellID   string
	SessionID string
}

// JobCompletion is what the agent is told when a job it launched exits.
type JobCompletion struct {
	ShellID     string
	SessionID   string
	Command     string
	Description string
	StartedAt   time.Time
	CompletedAt time.Time
	ExitCode    int
	Interrupted bool
	// Output is the tail of both streams; the full log stays in job_output.
	Output string
}

// Duration is how long the job ran.
func (c JobCompletion) Duration() time.Duration {
	if c.StartedAt.IsZero() || c.CompletedAt.IsZero() {
		return 0
	}
	return c.CompletedAt.Sub(c.StartedAt)
}

// Status is a one-word verdict for rendering.
func (c JobCompletion) Status() string {
	switch {
	case c.Interrupted:
		return "interrupted"
	case c.ExitCode != 0:
		return "failed"
	default:
		return "succeeded"
	}
}

const completionOutputLimit = 8000

// Oldest notices are dropped first; every job stays readable via job_output.
const maxPendingCompletionsPerSession = 50

// Nothing drains a session no agent watches (non-interactive, or
// notifications off), so cap the sessions tracked and evict oldest-first.
const maxMailboxSessions = 128

var (
	jobBroker     *pubsub.Broker[JobEvent]
	jobBrokerOnce sync.Once
)

func jobEvents() *pubsub.Broker[JobEvent] {
	jobBrokerOnce.Do(func() {
		jobBroker = pubsub.NewBroker[JobEvent]()
	})
	return jobBroker
}

// SubscribeJobEvents lets the UI render job state from pushed transitions
// instead of polling.
func SubscribeJobEvents(ctx context.Context) <-chan pubsub.Event[JobEvent] {
	return jobEvents().Subscribe(ctx)
}

func publishJobEvent(ev JobEvent) {
	jobEvents().Publish(pubsub.UpdatedEvent, ev)
}

// completionMailbox holds undelivered completions per owning session. It
// buffers because a job can finish mid-turn, between turns, or during
// shutdown, and this package should not have to know which.
type completionMailbox struct {
	mu sync.Mutex
	// order lists the tracked sessions oldest first, for eviction.
	order   []string
	pending map[string][]JobCompletion
}

var (
	jobMailbox     *completionMailbox
	jobMailboxOnce sync.Once
)

func mailbox() *completionMailbox {
	jobMailboxOnce.Do(func() {
		jobMailbox = &completionMailbox{pending: make(map[string][]JobCompletion)}
	})
	return jobMailbox
}

func (b *completionMailbox) add(c JobCompletion) {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue, tracked := b.pending[c.SessionID]
	if !tracked {
		b.order = append(b.order, c.SessionID)
		for len(b.order) > maxMailboxSessions {
			delete(b.pending, b.order[0])
			b.order = b.order[1:]
		}
	}
	queue = append(queue, c)
	if len(queue) > maxPendingCompletionsPerSession {
		queue = queue[len(queue)-maxPendingCompletionsPerSession:]
	}
	b.pending[c.SessionID] = queue
}

// forget drops a session's queue and its eviction slot.
func (b *completionMailbox) forget(sessionID string) {
	delete(b.pending, sessionID)
	b.order = slices.DeleteFunc(b.order, func(id string) bool { return id == sessionID })
}

func (b *completionMailbox) take(sessionID string) []JobCompletion {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue := b.pending[sessionID]
	b.forget(sessionID)
	return queue
}

// sessions returns the tracked sessions, oldest first.
func (b *completionMailbox) sessions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.order)
}

func (b *completionMailbox) count(sessionID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending[sessionID])
}

func (b *completionMailbox) clear(sessionID, shellID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue := b.pending[sessionID]
	kept := queue[:0]
	for _, c := range queue {
		if c.ShellID != shellID {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		b.forget(sessionID)
		return
	}
	b.pending[sessionID] = kept
}

// TakeJobCompletions removes and returns a session's notices, oldest
// first. Destructive, so exactly one caller delivers each notice.
func TakeJobCompletions(sessionID string) []JobCompletion {
	if sessionID == "" {
		return nil
	}
	return mailbox().take(sessionID)
}

// RestoreJobCompletions puts notices back after a failed delivery.
func RestoreJobCompletions(completions []JobCompletion) {
	for _, c := range completions {
		if c.SessionID != "" {
			mailbox().add(c)
		}
	}
}

// SessionsWithPendingCompletions lists the sessions currently holding
// undelivered notices. A watcher uses it to reconcile at startup: an
// event published before it subscribed is gone, and without this the
// notice would sit unread until some later completion for the same
// session happened to drain it.
func SessionsWithPendingCompletions() []string {
	return mailbox().sessions()
}

// PendingJobCompletions counts waiting notices without consuming them.
func PendingJobCompletions(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	return mailbox().count(sessionID)
}

// DiscardJobCompletion drops any pending notice for one job.
func DiscardJobCompletion(sessionID, shellID string) {
	if sessionID == "" || shellID == "" {
		return
	}
	mailbox().clear(sessionID, shellID)
}

// FormatJobCompletions renders notices for the model. The tag is greppable
// so it reads as automatic, not as something the user typed.
func FormatJobCompletions(completions []JobCompletion) string {
	var b strings.Builder
	for i, c := range completions {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "<background-job-finished id=%q status=%q exit_code=%d duration=%q>\n",
			c.ShellID, c.Status(), c.ExitCode, c.Duration().Round(time.Millisecond))
		fmt.Fprintf(&b, "command: %s\n", c.Command)
		if c.Description != "" {
			fmt.Fprintf(&b, "description: %s\n", c.Description)
		}
		if c.Output != "" {
			fmt.Fprintf(&b, "\n%s\n", c.Output)
		} else {
			b.WriteString("\n(no output)\n")
		}
		b.WriteString("</background-job-finished>")
	}
	return b.String()
}
