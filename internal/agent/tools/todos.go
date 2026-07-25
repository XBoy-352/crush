package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed todos.md
var todosDescription string

const TodosToolName = "todos"

type TodosParams struct {
	Todos []TodoItem `json:"todos" description:"The updated todo list. Pass an empty array to clear the list when all work is done."`
}

type TodoItem struct {
	Content    string `json:"content" description:"What needs to be done (imperative form)"`
	Status     string `json:"status" description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g., 'Running tests')"`
}

type TodosResponseMetadata struct {
	IsNew         bool           `json:"is_new"`
	Todos         []session.Todo `json:"todos"`
	JustCompleted []string       `json:"just_completed,omitempty"`
	JustStarted   string         `json:"just_started,omitempty"`
	Completed     int            `json:"completed"`
	Total         int            `json:"total"`
	// Cleared is true when the tool call emptied a previously non-empty list.
	Cleared bool `json:"cleared,omitempty"`
}

func NewTodosTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TodosToolName,
		todosDescription,
		func(ctx context.Context, params TodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for managing todos")
			}

			currentSession, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			hadTodos := len(currentSession.Todos) > 0
			isNew := !hadTodos
			oldStatusByContent := make(map[string]session.TodoStatus)
			for _, todo := range currentSession.Todos {
				oldStatusByContent[todo.Content] = todo.Status
			}

			for _, item := range params.Todos {
				switch item.Status {
				case "pending", "in_progress", "completed":
				default:
					return fantasy.ToolResponse{}, fmt.Errorf("invalid status %q for todo %q", item.Status, item.Content)
				}
			}

			todos := make([]session.Todo, len(params.Todos))
			var justCompleted []string
			var justStarted string
			completedCount := 0
			inProgressCount := 0

			for i, item := range params.Todos {
				todos[i] = session.Todo{
					Content:    item.Content,
					Status:     session.TodoStatus(item.Status),
					ActiveForm: item.ActiveForm,
				}

				newStatus := session.TodoStatus(item.Status)
				oldStatus, existed := oldStatusByContent[item.Content]

				switch newStatus {
				case session.TodoStatusCompleted:
					completedCount++
					if existed && oldStatus != session.TodoStatusCompleted {
						justCompleted = append(justCompleted, item.Content)
					}
				case session.TodoStatusInProgress:
					inProgressCount++
					if !existed || oldStatus != session.TodoStatusInProgress {
						if item.ActiveForm != "" {
							justStarted = item.ActiveForm
						} else {
							justStarted = item.Content
						}
					}
				}
			}

			// Persist nil rather than empty slice so JSON/DB treat a cleared
			// list the same as a session that never had todos.
			if len(todos) == 0 {
				currentSession.Todos = nil
			} else {
				currentSession.Todos = todos
			}
			_, err = sessions.Save(ctx, currentSession)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save todos: %w", err)
			}

			cleared := hadTodos && len(todos) == 0
			response := buildTodosResponse(todos, completedCount, inProgressCount, cleared)

			metadata := TodosResponseMetadata{
				IsNew:         isNew,
				Todos:         currentSession.Todos,
				JustCompleted: justCompleted,
				JustStarted:   justStarted,
				Completed:     completedCount,
				Total:         len(todos),
				Cleared:       cleared,
			}

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
		},
	)
}

// buildTodosResponse writes the model-facing tool result. When every item is
// completed it nudges the model to clear the list so the UI pill disappears.
func buildTodosResponse(todos []session.Todo, completedCount, inProgressCount int, cleared bool) string {
	if cleared {
		return "Todo list cleared. All work for this list is done."
	}

	pendingCount := 0
	for _, todo := range todos {
		if todo.Status == session.TodoStatusPending {
			pendingCount++
		}
	}

	response := "Todo list updated successfully.\n\n"
	response += fmt.Sprintf("Status: %d pending, %d in progress, %d completed\n",
		pendingCount, inProgressCount, completedCount)

	switch {
	case len(todos) == 0:
		// Empty update on an already-empty list.
		response += "Todo list is empty."
	case completedCount == len(todos):
		response += "All todos are completed. Call the todos tool once more with an empty todos array to clear the list so it no longer shows in the UI."
	case inProgressCount == 0 && pendingCount > 0:
		response += "No task is in_progress. Mark the next pending task in_progress before continuing, or complete remaining work and clear the list when finished."
	default:
		response += "Continue using the todo list to track progress. Mark the current task completed as soon as it is fully done, and clear the list with an empty todos array when the whole job is finished."
	}
	return response
}
