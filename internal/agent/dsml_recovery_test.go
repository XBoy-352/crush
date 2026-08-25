package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dsmlModel streams canned parts, simulating a provider whose leaked
// DSML markup arrives inside plain text deltas.
type dsmlModel struct {
	parts []fantasy.StreamPart
}

func (dsmlModel) Provider() string { return "test" }
func (dsmlModel) Model() string    { return "dsml" }
func (dsmlModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("unused")
}

func (dsmlModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unused")
}

func (dsmlModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unused")
}

func (m dsmlModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range m.parts {
			if !yield(part) {
				return
			}
		}
	}, nil
}

func viewTool(name string) fantasy.Tool {
	return fakeNamedTool{name: name}
}

type fakeNamedTool struct{ name string }

func (t fakeNamedTool) GetType() fantasy.ToolType { return fantasy.ToolTypeFunction }
func (t fakeNamedTool) GetName() string           { return t.name }

func collectStream(t *testing.T, m fantasy.LanguageModel, call fantasy.Call) []fantasy.StreamPart {
	t.Helper()
	stream, err := m.Stream(t.Context(), call)
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	return parts
}

func TestRetryableModel_DSMLRecovery_SpacedMarkupBecomesToolCall(t *testing.T) {
	t.Parallel()

	m := wrapRetryableModel(dsmlModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Now the scouting card:\n"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: `< | DSML | | DSML | tool_calls>`},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: `< | DSML | invoke name="view">`},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: `< | DSML | parameter name="file_path" string="true">/a/b.js</ | DSML | parameter>`},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: `</ | DSML | invoke></ | DSML | | DSML | tool_calls>`},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
		},
	}})

	call := fantasy.Call{Tools: []fantasy.Tool{viewTool("view")}}
	parts := collectStream(t, m, call)

	var text strings.Builder
	var toolCalls []fantasy.StreamPart
	for _, part := range parts {
		switch part.Type {
		case fantasy.StreamPartTypeTextStart, fantasy.StreamPartTypeTextEnd:
			// Structural parts pass through; nothing to assert on.
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeToolInputStart, fantasy.StreamPartTypeToolCall:
			toolCalls = append(toolCalls, part)
		case fantasy.StreamPartTypeFinish:
			assert.Equal(t, fantasy.FinishReasonToolCalls, part.FinishReason)
			assert.Equal(t, 10, int(part.Usage.TotalTokens), "usage must be preserved")
		default:
			t.Fatalf("unexpected part type %q in recovered stream", part.Type)
		}
	}

	assert.Equal(t, "Now the scouting card:\n", text.String())
	require.Len(t, toolCalls, 2)
	assert.Equal(t, "view", toolCalls[0].ToolCallName)
	assert.Equal(t, "view", toolCalls[1].ToolCallName)
	assert.JSONEq(t, `{"file_path": "/a/b.js"}`, toolCalls[1].ToolCallInput)
	assert.Equal(t, toolCalls[0].ID, toolCalls[1].ID, "input-start and tool-call share an id")
}

func TestRetryableModel_DSMLRecovery_NativeToolCallsBypassRecovery(t *testing.T) {
	t.Parallel()

	m := wrapRetryableModel(dsmlModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "using the real path"},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{Type: fantasy.StreamPartTypeToolInputStart, ID: "tc1", ToolCallName: "view"},
		{
			Type:          fantasy.StreamPartTypeToolCall,
			ID:            "tc1",
			ToolCallName:  "view",
			ToolCallInput: `{"file_path":"x"}`,
		},
		{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonToolCalls,
		},
	}})

	call := fantasy.Call{Tools: []fantasy.Tool{viewTool("view")}}
	parts := collectStream(t, m, call)

	// Native parts must pass through unmodified and in order.
	require.Len(t, parts, 6)
	assert.Equal(t, "using the real path", parts[1].Delta)
	assert.Equal(t, fantasy.FinishReasonToolCalls, parts[5].FinishReason)
}

func TestRetryableModel_DSMLRecovery_NoToolsMeansNoScan(t *testing.T) {
	t.Parallel()

	text := `< | DSML | invoke name="view">x</ | DSML | invoke>`
	m := wrapRetryableModel(dsmlModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: text},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
		},
	}})

	parts := collectStream(t, m, fantasy.Call{})
	require.Len(t, parts, 4)
	assert.Equal(t, text, parts[1].Delta, "text must be untouched without tools")
	assert.Equal(t, fantasy.FinishReasonStop, parts[3].FinishReason)
}

func TestRetryableModel_DSMLRecovery_UnknownToolKeepsText(t *testing.T) {
	t.Parallel()

	text := `discussing syntax: < | DSML | invoke name="not_a_tool">x</ | DSML | invoke>`
	m := wrapRetryableModel(dsmlModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: text},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
		},
	}})

	call := fantasy.Call{Tools: []fantasy.Tool{viewTool("view")}}
	parts := collectStream(t, m, call)

	for _, part := range parts {
		assert.NotEqual(t, fantasy.StreamPartTypeToolCall, part.Type)
		assert.NotEqual(t, fantasy.StreamPartTypeToolInputStart, part.Type)
	}
	assert.Equal(t, fantasy.FinishReasonStop, parts[len(parts)-1].FinishReason)
}

func TestRetryableModel_DSMLRecovery_StopWithoutMarkupFlushesText(t *testing.T) {
	t.Parallel()

	m := wrapRetryableModel(dsmlModel{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "hello "},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "world"},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
		},
	}})

	call := fantasy.Call{Tools: []fantasy.Tool{viewTool("view")}}
	parts := collectStream(t, m, call)

	require.Len(t, parts, 5)
	assert.Equal(t, "hello ", parts[1].Delta)
	assert.Equal(t, "world", parts[2].Delta)
	assert.Equal(t, fantasy.FinishReasonStop, parts[4].FinishReason)
}
