package model

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func newRetryTestUI(t *testing.T) *UI {
	t.Helper()
	com := &common.Common{}
	return &UI{
		com:    com,
		chat:   NewChat(com, ""),
		status: &Status{com: com},
	}
}

// TestClearRetryCountdownLeavesUnrelatedStatusAlone guards a state bug:
// clearRetryCountdown runs on every TypeAgentFinished / TypeAgentError,
// so it must be a no-op unless a countdown is actually live. A guard
// keyed on the sequence counter instead of the active status silently
// wiped the status bar on every turn after the session's first retry.
func TestClearRetryCountdownLeavesUnrelatedStatusAlone(t *testing.T) {
	t.Parallel()

	m := newRetryTestUI(t)

	// One retry happens and completes normally.
	m.beginRetryCountdown(notify.Notification{
		Type:       notify.TypeRetry,
		Message:    "rate limit",
		RetryDelay: 5 * time.Second,
		Attempt:    1,
		MaxRetries: 6,
	})
	require.Contains(t, m.status.msg.Msg, "Retrying in")
	m.clearRetryCountdown()
	require.Empty(t, m.status.msg.Msg, "countdown must be cleared once the retry resolves")

	// A later, unrelated status message must survive the next end-of-turn.
	m.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeInfo, Msg: "Copied to clipboard"})
	m.clearRetryCountdown()
	require.Equal(t, "Copied to clipboard", m.status.msg.Msg,
		"clearRetryCountdown wiped a status message while no countdown was active")
}

// TestRetryCountdownTicksAreSequenced ensures a stale tick from a
// superseded countdown cannot resurrect the status bar.
func TestRetryCountdownTicksAreSequenced(t *testing.T) {
	t.Parallel()

	m := newRetryTestUI(t)
	m.beginRetryCountdown(notify.Notification{
		Type: notify.TypeRetry, RetryDelay: 5 * time.Second, Attempt: 1, MaxRetries: 6,
	})
	stale := m.retrySeq
	m.clearRetryCountdown()
	require.Nil(t, m.handleRetryTick(retryTickMsg{seq: stale}))
	require.Empty(t, m.status.msg.Msg)
}
