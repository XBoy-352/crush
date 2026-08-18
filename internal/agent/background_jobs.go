package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/shell"
)

// jobNoticeDebounce batches completions that land close together into one
// notice, so three jobs finishing at once do not wake the agent three
// times.
const jobNoticeDebounce = 750 * time.Millisecond

// watchBackgroundJobs delivers background-job completions back to the
// agent that started them, so a job never has to be polled for.
//
// Delivery is a synthetic prompt through the normal dispatch path, which
// already handles both cases: a busy session queues it and folds it into
// the running turn's next step, and an idle session starts a new turn. A
// prompt queued during the final step of a turn is picked up by the
// end-of-run handoff rather than stranded.
func (c *coordinator) watchBackgroundJobs(ctx context.Context) {
	events := shell.SubscribeJobEvents(ctx)
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

// deliverJobCompletions drains a session's pending notices and hands them
// to the agent as one prompt. The drain is destructive, so a failed
// dispatch would lose the notice: on error the completions go back into
// the mailbox for the next attempt or the next turn.
//
// Ownership is checked before the drain, never after. The job registry and
// its notices are process-global while a server process runs a coordinator
// per workspace, so every watcher sees every completion. A watcher that
// drained first would take a sibling workspace's notice and then discard
// it for belonging to a session it has never heard of.
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
