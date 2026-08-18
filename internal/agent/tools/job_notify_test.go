package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

func waitForJobNotice(t *testing.T, sessionID string) []shell.JobCompletion {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if shell.PendingJobCompletions(sessionID) > 0 {
			return shell.TakeJobCompletions(sessionID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a background job notice for %q", sessionID)
	return nil
}

// A command the tool hands back as a background job announces itself when
// it exits, so the agent never has to poll job_output.
func TestBashBackgroundJobAnnouncesCompletion(t *testing.T) {
	sessionID := "session-announce"
	t.Cleanup(func() { shell.TakeJobCompletions(sessionID) })

	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)

	resp := runBashTool(t, tool, ctx, BashParams{
		Description:     "slow job",
		Command:         "sleep 1 && echo finished",
		RunInBackground: true,
	})
	require.Contains(t, resp.Content, "Background shell started with ID")

	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.NotEmpty(t, meta.ShellID)

	completions := waitForJobNotice(t, sessionID)
	require.Len(t, completions, 1)
	require.Equal(t, meta.ShellID, completions[0].ShellID)
	require.Equal(t, 0, completions[0].ExitCode)
	require.Contains(t, completions[0].Output, "finished")
}

// A command that finishes inside the foreground wait returns its output
// directly, so announcing it would repeat what the agent just read.
func TestBashForegroundCommandAnnouncesNothing(t *testing.T) {
	sessionID := "session-foreground"
	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "quick job",
		Command:     "echo quick",
	})
	require.Contains(t, resp.Content, "quick")

	// Give a wrongly-queued notice time to appear.
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, shell.PendingJobCompletions(sessionID))
}

// Collecting a finished job by hand drops its pending notice.
func TestJobOutputDiscardsPendingNotice(t *testing.T) {
	sessionID := "session-collect"
	t.Cleanup(func() { shell.TakeJobCompletions(sessionID) })

	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)

	resp := runBashTool(t, tool, ctx, BashParams{
		Description:     "collected job",
		Command:         "sleep 1 && echo collected",
		RunInBackground: true,
	})
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))

	input, err := json.Marshal(JobOutputParams{ShellID: meta.ShellID, Wait: true})
	require.NoError(t, err)
	out, err := NewJobOutputTool().Run(ctx, fantasy.ToolCall{ID: "c", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.Contains(t, out.Content, "collected")

	require.Zero(t, shell.PendingJobCompletions(sessionID))
}
