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
	// JobStarted fires when a shell is handed back to the agent as a
	// background job, not when it starts running: a command that finishes
	// inside the bash tool's foreground wait never becomes a visible job.
	JobStarted JobEventType = "started"
	// JobCompleted fires when a backgrounded shell exits.
	JobCompleted JobEventType = "completed"
	// JobRemoved fires when a job leaves the manager (killed or reaped).
	JobRemoved JobEventType = "removed"
)

// JobEvent describes one lifecycle transition. It carries no output, for
// the reasons documented on proto.BackgroundJob.
type JobEvent struct {
	Type        JobEventType
	ShellID     string
	SessionID   string
	Command     string
	Description string
	WorkingDir  string
	StartedAt   time.Time
	CompletedAt time.Time
	ExitCode    int
	Interrupted bool
}

// JobCompletion is the record handed to the agent when a background job it
// launched exits.
type JobCompletion struct {
	ShellID     string
	SessionID   string
	Command     string
	Description string
	WorkingDir  string
	StartedAt   time.Time
	CompletedAt time.Time
	ExitCode    int
	Interrupted bool
	// Output is the tail of the combined streams, capped at
	// completionOutputLimit. The full log stays behind job_output.
	Output string
}

// Duration is how long the job ran.
func (c JobCompletion) Duration() time.Duration {
	if c.StartedAt.IsZero() || c.CompletedAt.IsZero() {
		return 0
	}
	return c.CompletedAt.Sub(c.StartedAt)
}

// Succeeded reports whether the job exited cleanly.
func (c JobCompletion) Succeeded() bool {
	return c.ExitCode == 0 && !c.Interrupted
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

// maxPendingCompletionsPerSession bounds the mailbox; oldest notices are
// dropped first. Every job stays listed and readable via job_output.
const maxPendingCompletionsPerSession = 50

// maxMailboxSessions bounds how many sessions the mailbox tracks at once.
// Nothing drains a session whose agent never watches for completions (a
// non-interactive run, or notifications switched off), so without a cap a
// long-lived server process would accumulate a queue per session forever.
// Eviction is oldest-first by arrival.
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

// SubscribeJobEvents returns a channel of job lifecycle events, so the UI
// can render job state from pushed transitions instead of polling.
func SubscribeJobEvents(ctx context.Context) <-chan pubsub.Event[JobEvent] {
	return jobEvents().Subscribe(ctx)
}

func publishJobEvent(ev JobEvent) {
	jobEvents().Publish(pubsub.UpdatedEvent, ev)
}

// completionMailbox holds undelivered completions keyed by owning session.
// It buffers because a job can finish mid-turn (folded into the next
// step), between turns (wakes the agent), or during shutdown (dropped) —
// none of which the shell package should have to know about.
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

// TakeJobCompletions removes and returns a session's undelivered notices,
// oldest first. The drain is destructive so exactly one caller delivers
// each notice: folded into a step, or turned into a waking prompt.
func TakeJobCompletions(sessionID string) []JobCompletion {
	if sessionID == "" {
		return nil
	}
	return mailbox().take(sessionID)
}

// RestoreJobCompletions puts drained notices back, for a delivery attempt
// that failed after taking them.
func RestoreJobCompletions(completions []JobCompletion) {
	for _, c := range completions {
		if c.SessionID != "" {
			mailbox().add(c)
		}
	}
}

// PendingJobCompletions reports how many notices are waiting, without
// consuming them.
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

// FormatJobCompletions renders notices as the text handed to the model.
// The tag is greppable so the model can tell an automatic notice from
// something the user typed.
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
