package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/shell"
)

// jobNoticeDebounce batches near-simultaneous completions into one notice.
const jobNoticeDebounce = 750 * time.Millisecond

// watchBackgroundJobs delivers completions back to the agent that started
// the job, so nothing has to be polled for.
//
// Delivery is a synthetic prompt through the normal dispatch path, which
// already covers both cases: a busy session folds it into the running
// turn's next step, an idle one starts a turn. A prompt queued during a
// turn's final step is picked up by the end-of-run handoff.
// events must already be subscribed by the caller. Subscribing here
// would leave a gap: a job completing between the caller starting this
// goroutine and its first statement would publish to no subscriber, and
// its notice would sit unread until an unrelated completion for the same
// session drained it.
func (c *coordinator) watchBackgroundJobs(ctx context.Context, events <-chan pubsub.Event[shell.JobEvent]) {
	pending := make(map[string]struct{})
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			job := ev.Payload
			if job.Type != shell.JobCompleted || job.SessionID == "" {
				continue
			}
			pending[job.SessionID] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(jobNoticeDebounce)
				timerC = timer.C
			}
		case <-timerC:
			timer, timerC = nil, nil
			for sessionID := range pending {
				delete(pending, sessionID)
				c.deliverJobCompletions(ctx, sessionID)
			}
		}
	}
}

// deliverJobCompletions drains a session's notices into one prompt. The
// drain is destructive, so a failed dispatch puts them back.
//
// Ownership is checked before the drain, never after: notices are
// process-global while a server runs a coordinator per workspace, so a
// watcher that drained first would take a sibling's notice and discard it
// for belonging to a session it has never heard of.
func (c *coordinator) deliverJobCompletions(ctx context.Context, sessionID string) {
	if _, err := c.sessions.Get(ctx, sessionID); err != nil {
		slog.Debug("Ignoring background job notice for a session this agent does not own", "session", sessionID, "error", err)
		return
	}
	completions := shell.TakeJobCompletions(sessionID)
	if len(completions) == 0 {
		return
	}

	prompt := shell.FormatJobCompletions(completions)
	go func() {
		if _, err := c.run(ctx, nil, sessionID, prompt, runOptions{synthetic: true}); err != nil {
			slog.Error("Failed to deliver background job completion", "session", sessionID, "error", err)
			shell.RestoreJobCompletions(completions)
		}
	}()
}
