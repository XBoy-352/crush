package tools

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBuildTodosResponse(t *testing.T) {
	t.Parallel()

	t.Run("cleared list", func(t *testing.T) {
		t.Parallel()
		got := buildTodosResponse(nil, 0, 0, true)
		require.Contains(t, got, "Todo list cleared")
		require.NotContains(t, got, "empty todos array")
	})

	t.Run("all completed nudges clear", func(t *testing.T) {
		t.Parallel()
		todos := []session.Todo{
			{Content: "a", Status: session.TodoStatusCompleted},
			{Content: "b", Status: session.TodoStatusCompleted},
		}
		got := buildTodosResponse(todos, 2, 0, false)
		require.Contains(t, got, "2 completed")
		require.Contains(t, got, "empty todos array")
	})

	t.Run("no in_progress with pending work", func(t *testing.T) {
		t.Parallel()
		todos := []session.Todo{
			{Content: "a", Status: session.TodoStatusCompleted},
			{Content: "b", Status: session.TodoStatusPending},
		}
		got := buildTodosResponse(todos, 1, 0, false)
		require.Contains(t, got, "No task is in_progress")
	})

	t.Run("healthy in_progress list", func(t *testing.T) {
		t.Parallel()
		todos := []session.Todo{
			{Content: "a", Status: session.TodoStatusCompleted},
			{Content: "b", Status: session.TodoStatusInProgress, ActiveForm: "Doing b"},
		}
		got := buildTodosResponse(todos, 1, 1, false)
		require.Contains(t, got, "1 in progress")
		require.Contains(t, got, "clear the list")
		require.NotContains(t, got, "All todos are completed")
	})
}
