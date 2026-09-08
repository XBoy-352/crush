package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/childjobs"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartSubAgent_ReturnsImmediatelyWhileChildRuns(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	started := make(chan struct{})
	release := make(chan struct{})
	agent := newMockAgent(providerID, 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer close(release)

	// The parent's own agent: the child runs on a separate instance, so
	// nothing the child does may mark the parent busy.
	coord.currentAgent = newMockAgent(providerID, 4096, nil)
	// Stub notice dispatch: the minimal test coordinator cannot run the
	// full parent turn path.
	coord.deliverNotice = func(parentSessionID, prompt string) {}

	jobID, err := coord.startSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "long task",
		SessionTitle:   "Async Test",
	}, childjobs.KindAgent)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(jobID, "agent-"))

	<-started
	// The parent session stays chat-able while the child runs: the child
	// runs on a separate SessionAgent instance, so the parent is never
	// marked busy by the child's work.
	assert.False(t, coord.currentAgent.IsSessionBusy(parentSession.ID))

	job, ok := childjobs.GetRegistry().Get(jobID)
	require.True(t, ok)
	require.NoError(t, childjobs.GetRegistry().Kill(jobID))
	_ = job
}

func TestChildJobNoticeText_Completed(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)
	// Stub notice dispatch: the minimal test coordinator cannot run the
	// full parent turn path; the envelope itself is asserted below.
	coord.deliverNotice = func(parentSessionID, prompt string) {}

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("exploration finished"), nil
	})

	jobID, err := coord.startSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "explore",
		SessionTitle:   "Notice Test",
	}, childjobs.KindAgent)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		job, ok := childjobs.GetRegistry().Get(jobID)
		return ok && job.Done()
	}, 5*time.Second, 10*time.Millisecond)

	job, _ := childjobs.GetRegistry().Get(jobID)
	notice := childJobNoticeText(job)
	assert.Contains(t, notice, "<task-notification>")
	assert.Contains(t, notice, "<status>completed</status>")
	assert.Contains(t, notice, "exploration finished")
	assert.Contains(t, notice, "[SYSTEM NOTIFICATION - NOT USER INPUT]")
	assert.Contains(t, notice, jobID)
}

func TestChildJobNoticeText_KilledAndError(t *testing.T) {
	r := childjobs.GetRegistry()
	parent := t.Name()

	killedJob := &childjobs.Job{Kind: childjobs.KindAgent, ParentSessionID: parent, Title: "Explore"}
	require.NoError(t, r.Start(context.Background(), killedJob, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, nil))
	require.NoError(t, r.Kill(killedJob.ID))
	require.Eventually(t, func() bool { return killedJob.Done() }, time.Second, 5*time.Millisecond)
	assert.Contains(t, childJobNoticeText(killedJob), "<status>killed</status>")
	assert.Contains(t, childJobNoticeText(killedJob), "was stopped")
	assert.NotContains(t, childJobNoticeText(killedJob), "<result>")

	errorJob := &childjobs.Job{Kind: childjobs.KindAgent, ParentSessionID: parent, Title: "Explore"}
	require.NoError(t, r.Start(context.Background(), errorJob, func(ctx context.Context) (string, error) {
		return "", errors.New("provider down")
	}, nil))
	require.Eventually(t, func() bool { return errorJob.Done() }, time.Second, 5*time.Millisecond)
	notice := childJobNoticeText(errorJob)
	assert.Contains(t, notice, "<status>failed</status>")
	assert.Contains(t, notice, "provider down")
	assert.NotContains(t, notice, "<result>")
}

func TestCreateUserMessage_NoticeRole(t *testing.T) {
	env := testEnv(t)
	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	a := &sessionAgent{messages: env.messages}

	msg, err := a.createUserMessage(t.Context(), SessionAgentCall{
		SessionID: parentSession.ID,
		Prompt:    "<system_reminder>notice</system_reminder>",
		Notice:    true,
	})
	require.NoError(t, err)
	assert.Equal(t, message.Notice, msg.Role)

	msg, err = a.createUserMessage(t.Context(), SessionAgentCall{
		SessionID: parentSession.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)
	assert.Equal(t, message.User, msg.Role)
}

func TestToAIMessage_NoticeMapsToUserRole(t *testing.T) {
	msg := &message.Message{
		Role: message.Notice,
		Parts: []message.ContentPart{
			message.TextContent{Text: "<task-notification>hi</task-notification>"},
		},
	}
	aiMsgs := msg.ToAIMessage()
	require.Len(t, aiMsgs, 1)
	assert.Equal(t, fantasy.MessageRoleUser, aiMsgs[0].Role)
}

func TestHasUserTextMessage_ExcludesNotices(t *testing.T) {
	assert.False(t, hasUserTextMessage(nil))
	assert.False(t, hasUserTextMessage([]message.Message{{
		Role:  message.Notice,
		Parts: []message.ContentPart{message.TextContent{Text: "job finished"}},
	}}))
	assert.True(t, hasUserTextMessage([]message.Message{{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	}}))
}
