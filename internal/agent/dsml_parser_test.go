package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const spacedDSMLSample = `Now the scouting card:
< | DSML | | DSML | tool_calls>
  < | DSML | invoke name="view">
    < | DSML | parameter name="file_path" string="true">/path/to/file.js</ | DSML | parameter>
    < | DSML | parameter name="limit" string="false">10</ | DSML | parameter>
    < | DSML | parameter name="offset" string="false">1420</ | DSML | parameter>
  </ | DSML | invoke>
</ | DSML | | DSML | tool_calls>`

func TestExtractLeakedToolCalls_SpacedDSML(t *testing.T) {
	t.Parallel()
	calls, clean := ExtractLeakedToolCalls(spacedDSMLSample, []string{"view", "edit"})
	require.Len(t, calls, 1)
	assert.Equal(t, "view", calls[0].Name)
	assert.Equal(t, map[string]any{
		"file_path": "/path/to/file.js",
		"limit":     10,
		"offset":    1420,
	}, calls[0].Params)
	assert.Equal(t, "Now the scouting card:", strings.TrimSpace(clean))
}

func TestExtractLeakedToolCalls_MultipleInvokesSpacedDSML(t *testing.T) {
	t.Parallel()
	text := `< | DSML | | DSML | tool_calls>` +
		`< | DSML | invoke name="bash">` +
		`< | DSML | parameter name="command" string="true">ls -la</ | DSML | parameter>` +
		`</ | DSML | invoke>` +
		`< | DSML | invoke name="glob">` +
		`< | DSML | parameter name="pattern" string="true">**/*.go</ | DSML | parameter>` +
		`</ | DSML | invoke>` +
		`</ | DSML | | DSML | tool_calls>`
	calls, clean := ExtractLeakedToolCalls(text, []string{"bash", "glob"})
	require.Len(t, calls, 2)
	assert.Equal(t, "bash", calls[0].Name)
	assert.Equal(t, map[string]any{"command": "ls -la"}, calls[0].Params)
	assert.Equal(t, "glob", calls[1].Name)
	assert.Empty(t, clean)
}

func TestExtractLeakedToolCalls_CompactDSML(t *testing.T) {
	t.Parallel()
	text := `Reading the file.<｜tool calls｜><｜invoke:view｜>` +
		`<｜parameter name="file_path" string="true"｜>main.go<｜/parameter｜>` +
		`<｜parameter name="limit" string="false"｜>50<｜/parameter｜>` +
		`<｜/invoke｜><｜/tool calls｜>`
	calls, clean := ExtractLeakedToolCalls(text, []string{"view"})
	require.Len(t, calls, 1)
	assert.Equal(t, "view", calls[0].Name)
	assert.Equal(t, map[string]any{"file_path": "main.go", "limit": 50}, calls[0].Params)
	assert.Equal(t, "Reading the file.", clean)
}

func TestExtractLeakedToolCalls_GenericToolCallBlock(t *testing.T) {
	t.Parallel()
	text := `Let me check.<tool_call>{"name": "bash", "arguments": {"command": "go test ./..."}}</tool_call>`
	calls, clean := ExtractLeakedToolCalls(text, []string{"bash"})
	require.Len(t, calls, 1)
	assert.Equal(t, "bash", calls[0].Name)
	assert.Equal(t, map[string]any{"command": "go test ./..."}, calls[0].Params)
	assert.Equal(t, "Let me check.", clean)
}

func TestExtractLeakedToolCalls_GenericParametersKey(t *testing.T) {
	t.Parallel()
	text := `<tool_call>{"name": "grep", "parameters": {"pattern": "foo"}}</tool_call>`
	calls, _ := ExtractLeakedToolCalls(text, []string{"grep"})
	require.Len(t, calls, 1)
	assert.Equal(t, map[string]any{"pattern": "foo"}, calls[0].Params)
}

func TestExtractLeakedToolCalls_ParameterTypes(t *testing.T) {
	t.Parallel()
	text := `< | DSML | | DSML | tool_calls>` +
		`< | DSML | invoke name="edit">` +
		`< | DSML | parameter name="count" string="false">42</ | DSML | parameter>` +
		`< | DSML | parameter name="ratio" string="false">0.5</ | DSML | parameter>` +
		`< | DSML | parameter name="force" string="false">true</ | DSML | parameter>` +
		`< | DSML | parameter name="label">plain</ | DSML | parameter>` +
		`</ | DSML | invoke>` +
		`</ | DSML | | DSML | tool_calls>`
	calls, _ := ExtractLeakedToolCalls(text, []string{"edit"})
	require.Len(t, calls, 1)
	params := calls[0].Params
	assert.Equal(t, 42, params["count"])
	assert.Equal(t, 0.5, params["ratio"])
	assert.Equal(t, true, params["force"])
	assert.Equal(t, "plain", params["label"])
}

func TestExtractLeakedToolCalls_UnknownToolNoFalsePositive(t *testing.T) {
	t.Parallel()
	// The model is discussing DSML syntax in text: none of these names
	// are available tools, so nothing may be extracted or stripped.
	calls, clean := ExtractLeakedToolCalls(spacedDSMLSample, []string{"bash", "read"})
	assert.Nil(t, calls)
	assert.Equal(t, spacedDSMLSample, clean)
}

func TestExtractLeakedToolCalls_PlainTextUntouched(t *testing.T) {
	t.Parallel()
	text := "Just a normal answer with <html> tags and no markup blocks."
	calls, clean := ExtractLeakedToolCalls(text, []string{"bash"})
	assert.Nil(t, calls)
	assert.Equal(t, text, clean)
}

func TestExtractLeakedToolCalls_EmptyAvailableTools(t *testing.T) {
	t.Parallel()
	calls, clean := ExtractLeakedToolCalls(spacedDSMLSample, nil)
	assert.Nil(t, calls)
	assert.Equal(t, spacedDSMLSample, clean)
}

func TestMarshalLeakedToolCall_NilParams(t *testing.T) {
	t.Parallel()
	input, err := MarshalLeakedToolCall(LeakedToolCall{Name: "view"})
	require.NoError(t, err)
	assert.Equal(t, "{}", input)
}
