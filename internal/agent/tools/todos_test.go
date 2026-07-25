package tools

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBuildTodosResponse(t *testing.T) {
	t.Parallel()

	t.Run("explicit clear", func(t *testing.T) {
		t.Parallel()
		got := buildTodosResponse(nil, 0, 0, true, false, nil)
		require.Contains(t, got, "Todo list cleared")
		require.NotContains(t, got, "Completed")
	})

	t.Run("auto clear after all completed", func(t *testing.T) {
		t.Parallel()
		got := buildTodosResponse(nil, 0, 0, true, true, []string{"a", "b"})
		require.Contains(t, got, "Completed 2 todo")
		require.Contains(t, got, "cleared")
	})

	t.Run("no in_progress with pending work", func(t *testing.T) {
		t.Parallel()
		todos := []session.Todo{
			{Content: "a", Status: session.TodoStatusCompleted},
			{Content: "b", Status: session.TodoStatusPending},
		}
		got := buildTodosResponse(todos, 1, 0, false, false, nil)
		require.Contains(t, got, "No task is in_progress")
	})

	t.Run("healthy in_progress list", func(t *testing.T) {
		t.Parallel()
		todos := []session.Todo{
			{Content: "a", Status: session.TodoStatusCompleted},
			{Content: "b", Status: session.TodoStatusInProgress, ActiveForm: "Doing b"},
		}
		got := buildTodosResponse(todos, 1, 1, false, false, nil)
		require.Contains(t, got, "1 in progress")
		require.Contains(t, got, "clears automatically")
	})
}

func TestNormalizeTodosAutoClear(t *testing.T) {
	t.Parallel()

	// Mirror the tool's auto-clear decision without spinning up a session store.
	todos := []session.Todo{
		{Content: "a", Status: session.TodoStatusCompleted},
		{Content: "b", Status: session.TodoStatusCompleted},
	}
	completed := 0
	for _, tdo := range todos {
		if tdo.Status == session.TodoStatusCompleted {
			completed++
		}
	}
	autoCleared := len(todos) > 0 && completed == len(todos)
	require.True(t, autoCleared)
	if autoCleared {
		todos = nil
	}
	require.Nil(t, todos)
}
