package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAggregateSessionCostByModel(t *testing.T) {
	t.Parallel()

	mk := func(model, provider string, prompt, completion int64, cost float64) *message.Message {
		m := &message.Message{
			Role:     message.Assistant,
			Model:    model,
			Provider: provider,
		}
		m.AddFinishWithUsage(message.FinishReasonEndTurn, "", "", prompt, completion, cost)
		return m
	}

	rows := aggregateSessionCostByModel([]*message.Message{
		mk("large-a", "prov", 100, 50, 0.01),
		mk("small-b", "prov", 10, 5, 0.001),
		mk("large-a", "prov", 200, 80, 0.02),
		{Role: message.User},                                         // ignored
		{Role: message.Assistant, Model: "legacy", Provider: "prov"}, // count only
	})
	require.Len(t, rows, 3)

	require.Equal(t, "large-a", rows[0].Model)
	require.Equal(t, int64(2), rows[0].MessageCount)
	require.Equal(t, int64(300), rows[0].PromptTokens)
	require.Equal(t, int64(130), rows[0].CompletionTokens)
	require.InDelta(t, 0.03, rows[0].Cost, 1e-9)

	require.Equal(t, "small-b", rows[1].Model)
	require.Equal(t, int64(1), rows[1].MessageCount)
	require.Equal(t, int64(10), rows[1].PromptTokens)

	require.Equal(t, "legacy", rows[2].Model)
	require.Equal(t, int64(1), rows[2].MessageCount)
	require.Equal(t, int64(0), rows[2].PromptTokens)
}

func TestOutputSessionByModelJSON(t *testing.T) {
	t.Parallel()
	m := &message.Message{Role: message.Assistant, Model: "m1", Provider: "p1"}
	m.AddFinishWithUsage(message.FinishReasonEndTurn, "", "", 11, 7, 0.5)

	var buf bytes.Buffer
	err := outputSessionByModelJSON(&buf, session.Session{
		ID: "sess-1", Title: "t", Cost: 0.5, PromptTokens: 11, CompletionTokens: 7,
	}, []*message.Message{m})
	require.NoError(t, err)

	var out sessionShowOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	require.Len(t, out.Meta.ByModel, 1)
	require.Equal(t, "m1", out.Meta.ByModel[0].Model)
	require.Equal(t, int64(11), out.Meta.ByModel[0].PromptTokens)
	require.InDelta(t, 0.5, out.Meta.ByModel[0].Cost, 1e-9)
}
