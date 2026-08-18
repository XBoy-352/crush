package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

// waitForJobNotice polls the session's messages for the completion notice.
// The delivery is asynchronous by design: the job finishes on its own
// goroutine and the notice reaches the agent through the normal dispatch
// path.
func waitForJobNotice(t *testing.T, coord *coordinator, sessionID string) message.Message {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := coord.messages.List(t.Context(), sessionID)
		require.NoError(t, err)
		for _, m := range msgs {
			if m.Role == message.User && strings.Contains(m.Content().Text, "<background-job-finished") {
				return m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a background job completion notice")
	return message.Message{}
}

// A job that finishes while the session is idle must reach the agent on
// its own. Nothing here submits a prompt: the completion is the only
// trigger, which is the whole point of the notice.
func TestBackgroundJobCompletionWakesIdleAgent(t *testing.T) {
	coord := newGateTestCoordinator(t, true)

	sess, err := coord.sessions.Create(t.Context(), "job notice")
	require.NoError(t, err)
	t.Cleanup(func() { shell.TakeJobCompletions(sess.ID) })

	go coord.watchBackgroundJobs(t.Context())

	manager := shell.GetBackgroundShellManager()
	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo built && exit 2", "build")
	require.NoError(t, err)
	t.Cleanup(func() { manager.Remove(bgShell.ID) })
	bgShell.MarkBackgrounded(sess.ID)

	notice := waitForJobNotice(t, coord, sess.ID)
	text := notice.Content().Text
	require.Contains(t, text, bgShell.ID)
	require.Contains(t, text, `status="failed"`)
	require.Contains(t, text, "exit_code=2")
	require.Contains(t, text, "built")
	require.Zero(t, shell.PendingJobCompletions(sess.ID))
}

// Jobs finishing together are batched into one notice rather than waking
// the agent once per job.
func TestBackgroundJobCompletionsAreBatched(t *testing.T) {
	coord := newGateTestCoordinator(t, true)

	sess, err := coord.sessions.Create(t.Context(), "batched notices")
	require.NoError(t, err)
	t.Cleanup(func() { shell.TakeJobCompletions(sess.ID) })

	go coord.watchBackgroundJobs(t.Context())

	manager := shell.GetBackgroundShellManager()
	var ids []string
	for range 3 {
		bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo done", "")
		require.NoError(t, err)
		t.Cleanup(func() { manager.Remove(bgShell.ID) })
		bgShell.MarkBackgrounded(sess.ID)
		ids = append(ids, bgShell.ID)
	}

	notice := waitForJobNotice(t, coord, sess.ID)
	for _, id := range ids {
		require.Contains(t, notice.Content().Text, id)
	}
}

// A notice for a session that no longer exists is dropped rather than
// dispatched against a missing session.
func TestBackgroundJobCompletionForUnknownSessionIsDropped(t *testing.T) {
	coord := newGateTestCoordinator(t, true)

	shell.RestoreJobCompletions([]shell.JobCompletion{{
		ShellID:   "0FF",
		SessionID: "does-not-exist",
		Command:   "true",
	}})

	coord.deliverJobCompletions(t.Context(), "does-not-exist")
	require.Zero(t, shell.PendingJobCompletions("does-not-exist"))
}
