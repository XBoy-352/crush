package shell

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitForCompletions polls the mailbox because the notice is queued by the
// shell's exit goroutine, not by the caller that started it.
func waitForCompletions(t *testing.T, sessionID string, want int) []JobCompletion {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if PendingJobCompletions(sessionID) >= want {
			return TakeJobCompletions(sessionID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d completion(s) for %q, have %d", want, sessionID, PendingJobCompletions(sessionID))
	return nil
}

func TestBackgroundedShellQueuesCompletion(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	sessionID := t.Name()
	t.Cleanup(func() { TakeJobCompletions(sessionID) })

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hello && exit 3", "greeting")
	require.NoError(t, err)
	bgShell.MarkBackgrounded(sessionID)

	completions := waitForCompletions(t, sessionID, 1)
	require.Len(t, completions, 1)
	c := completions[0]
	require.Equal(t, bgShell.ID, c.ShellID)
	require.Equal(t, sessionID, c.SessionID)
	require.Equal(t, 3, c.ExitCode)
	require.False(t, c.Succeeded())
	require.Equal(t, "failed", c.Status())
	require.Contains(t, c.Output, "hello")
	require.Equal(t, "greeting", c.Description)
}

// A command that finishes inside the bash tool's foreground wait is never
// marked, and must not be announced: the tool already returned its output.
func TestUnmarkedShellQueuesNoCompletion(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	sessionID := t.Name()

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo quick", "")
	require.NoError(t, err)
	bgShell.Wait()

	require.Zero(t, PendingJobCompletions(sessionID))
	require.Empty(t, bgShell.SessionID())
}

// The shell can exit before the tool decides to hand it back. The notice
// still has to be queued, otherwise a job that finishes in that window is
// silently lost.
func TestCompletionQueuedWhenShellExitsBeforeHandoff(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	sessionID := t.Name()
	t.Cleanup(func() { TakeJobCompletions(sessionID) })

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo done", "")
	require.NoError(t, err)
	bgShell.Wait()
	require.Zero(t, PendingJobCompletions(sessionID))

	bgShell.MarkBackgrounded(sessionID)

	completions := waitForCompletions(t, sessionID, 1)
	require.Len(t, completions, 1)
	require.Equal(t, 0, completions[0].ExitCode)
	require.True(t, completions[0].Succeeded())
}

func TestDiscardedShellQueuesNoCompletion(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	sessionID := t.Name()

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 10", "")
	require.NoError(t, err)
	bgShell.MarkBackgrounded(sessionID)

	require.NoError(t, manager.Kill(bgShell.ID))
	require.Zero(t, PendingJobCompletions(sessionID))
}

// A notice already queued when the agent collects the result itself is
// dropped, so it is not told twice.
func TestDiscardClearsQueuedCompletion(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	sessionID := t.Name()
	t.Cleanup(func() { TakeJobCompletions(sessionID) })

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hi", "")
	require.NoError(t, err)
	bgShell.MarkBackgrounded(sessionID)
	waitForPending(t, sessionID, 1)

	bgShell.DiscardCompletionNotice()
	require.Zero(t, PendingJobCompletions(sessionID))
}

func waitForPending(t *testing.T, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if PendingJobCompletions(sessionID) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending completion(s) for %q", want, sessionID)
}

func TestTakeJobCompletionsIsPerSession(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	mine, theirs := t.Name()+"-a", t.Name()+"-b"
	t.Cleanup(func() {
		TakeJobCompletions(mine)
		TakeJobCompletions(theirs)
	})

	a, err := manager.Start(t.Context(), t.TempDir(), nil, "echo a", "")
	require.NoError(t, err)
	a.MarkBackgrounded(mine)
	b, err := manager.Start(t.Context(), t.TempDir(), nil, "echo b", "")
	require.NoError(t, err)
	b.MarkBackgrounded(theirs)

	waitForPending(t, mine, 1)
	waitForPending(t, theirs, 1)

	require.Len(t, TakeJobCompletions(mine), 1)
	require.Zero(t, PendingJobCompletions(mine))
	require.Len(t, TakeJobCompletions(theirs), 1)
}

func TestRunningCountsSplitsBySession(t *testing.T) {
	t.Parallel()
	manager := newBackgroundShellManager()
	mine, theirs := t.Name()+"-a", t.Name()+"-b"

	a, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 10", "")
	require.NoError(t, err)
	a.MarkBackgrounded(mine)
	b, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 10", "")
	require.NoError(t, err)
	b.MarkBackgrounded(theirs)
	t.Cleanup(func() {
		manager.Kill(a.ID)
		manager.Kill(b.ID)
	})

	own, other := manager.RunningCounts(mine)
	require.Equal(t, 1, own)
	require.Equal(t, 1, other)
}

func TestFormatJobCompletions(t *testing.T) {
	t.Parallel()
	start := time.Now()
	out := FormatJobCompletions([]JobCompletion{
		{
			ShellID:     "00A",
			Command:     "go build ./...",
			Description: "build",
			StartedAt:   start,
			CompletedAt: start.Add(2 * time.Second),
			ExitCode:    1,
			Output:      "boom",
		},
		{
			ShellID:     "00B",
			Command:     "true",
			StartedAt:   start,
			CompletedAt: start,
		},
	})

	require.Contains(t, out, `<background-job-finished id="00A" status="failed" exit_code=1 duration="2s">`)
	require.Contains(t, out, "command: go build ./...")
	require.Contains(t, out, "description: build")
	require.Contains(t, out, "boom")
	require.Contains(t, out, `<background-job-finished id="00B" status="succeeded" exit_code=0`)
	require.Contains(t, out, "(no output)")
}

func TestCompletionOutputKeepsTail(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", completionOutputLimit+500) + "VERDICT"
	out := completionOutput(long, "")
	require.Less(t, len(out), len(long))
	require.Contains(t, out, "VERDICT")
	require.Contains(t, out, "output truncated")
}
