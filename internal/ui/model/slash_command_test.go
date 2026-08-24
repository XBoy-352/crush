package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

type slashWorkspace struct {
	workspace.Workspace
	prompt string
}

func (slashWorkspace) Config() *config.Config {
	return &config.Config{}
}

func (slashWorkspace) PermissionSkipRequests() bool {
	return false
}

func (slashWorkspace) PermissionPlanMode() bool {
	return false
}

func (w slashWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return w.prompt, nil
}

func TestHandleSlashCommand_MatchesMCPPrompt(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.mcpPrompts = []commands.MCPPrompt{
		{
			ID:       "ponytail:ponytail",
			Title:    "Ponytail mode",
			PromptID: "ponytail",
			ClientID: "ponytail",
		},
	}
	u.com.Workspace = slashWorkspace{prompt: "ponytail rules"}

	// A matching "/ponytail <text>" must return a command (not nil) so the
	// message is routed through the MCP prompt instead of being sent literally.
	cmd := u.handleSlashCommand("/ponytail create a branch", nil)
	require.NotNil(t, cmd)

	// Executing the command must fetch the prompt and send the trailing text
	// as the message body with the instructions prepended.
	msg := cmd()
	require.IsType(t, sendMessageMsg{}, msg)
	sm := msg.(sendMessageMsg)
	require.Equal(t, "ponytail rules\n\ncreate a branch", sm.Content)
}

func TestHandleSlashCommand_UnknownCommandFallsThrough(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.mcpPrompts = []commands.MCPPrompt{
		{ID: "ponytail:ponytail", PromptID: "ponytail", ClientID: "ponytail"},
	}

	// Unknown commands must return nil so the caller sends the text normally.
	require.Nil(t, u.handleSlashCommand("/nonexistent do something", nil))
	// Plain text (no slash) must also fall through.
	require.Nil(t, u.handleSlashCommand("just a normal message", nil))
	// A bare "/" with no command name must fall through.
	require.Nil(t, u.handleSlashCommand("/", nil))
}

func TestHandleSlashCommand_MatchesCustomCommand(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.customCommands = []commands.CustomCommand{
		{ID: "user:review", Name: "review", Content: "Review my changes."},
	}

	cmd := u.handleSlashCommand("/review", nil)
	require.NotNil(t, cmd)

	// Executing the custom command sends the content as the message.
	msg := cmd()
	require.IsType(t, sendMessageMsg{}, msg)
	require.Equal(t, "Review my changes.", msg.(sendMessageMsg).Content)
}

func TestHasRequiredArgs(t *testing.T) {
	t.Parallel()

	require.False(t, hasRequiredArgs(nil))
	require.False(t, hasRequiredArgs([]commands.Argument{{ID: "mode", Required: false}}))
	require.True(t, hasRequiredArgs([]commands.Argument{{ID: "mode", Required: true}}))
	require.True(t, hasRequiredArgs([]commands.Argument{
		{ID: "mode", Required: false},
		{ID: "file", Required: true},
	}))
}
