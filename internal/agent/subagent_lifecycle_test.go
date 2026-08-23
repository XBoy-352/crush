package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifecycleSpy wires a fresh notification broker to a background pump and
// returns the publisher for the coordinator plus a channel of
// SubAgentLifecycle payloads. stop cancels the subscription and blocks until
// the pump has exited, so no goroutine leaks into goleak.
func lifecycleSpy(t *testing.T) (pubsub.Publisher[notify.Notification], <-chan *notify.SubAgentLifecycle, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	broker := pubsub.NewBroker[notify.Notification]()
	sub := broker.Subscribe(ctx)
	events := make(chan *notify.SubAgentLifecycle, 16)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for ev := range sub {
			if ev.Payload.Type == notify.TypeSubAgentLifecycle && ev.Payload.SubAgentLifecycle != nil {
				events <- ev.Payload.SubAgentLifecycle
			}
		}
		close(events)
	}()
	stop := func() {
		cancel()
		<-exited
	}
	t.Cleanup(stop)
	return broker, events, stop
}

func TestRunSubAgent_LifecycleEvents(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("emits start then done", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		publisher, events, _ := lifecycleSpy(t)
		coord.notify = publisher

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		require.False(t, resp.IsError)

		start := <-events
		assert.Equal(t, "start", start.Phase)
		assert.Equal(t, parentSession.ID, start.ParentSessionID)
		assert.Equal(t, "call-1", start.ToolCallID)
		assert.Equal(t, "Test", start.Title)
		require.NotEmpty(t, start.SubSessionID)

		end := <-events
		assert.Equal(t, "done", end.Phase)
		assert.Equal(t, start.SubSessionID, end.SubSessionID,
			"start and done must reference the same sub-session")
	})

	t.Run("emits start then error on failure", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		publisher, events, _ := lifecycleSpy(t)
		coord.notify = publisher

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("provider request failed")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		require.True(t, resp.IsError)

		start := <-events
		assert.Equal(t, "start", start.Phase)

		fail := <-events
		assert.Equal(t, "error", fail.Phase)
		assert.Contains(t, fail.Error, "provider request failed")
		assert.Equal(t, start.SubSessionID, fail.SubSessionID)
	})

	t.Run("nil broker does not panic", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
	})
}

func TestListChildrenSeedsPanel(t *testing.T) {
	env := testEnv(t)

	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
	require.NoError(t, err)

	children, err := env.sessions.ListChildren(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, child.ID, children[0].ID)
	assert.Equal(t, "Child", children[0].Title)
}
