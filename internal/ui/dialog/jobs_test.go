package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// startTestJobs registers n background shells in the process-global manager
// and removes them again afterwards. The commands never have to run: the
// dialog only reads the registry, so this stays hermetic and portable.
func startTestJobs(t *testing.T, n int) []string {
	t.Helper()

	manager := shell.GetBackgroundShellManager()
	ids := make([]string, 0, n)
	for range n {
		bs, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 30", "")
		require.NoError(t, err)
		ids = append(ids, bs.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = manager.Kill(id)
			_ = manager.Remove(id)
		}
	})
	return ids
}

func newTestJobs(t *testing.T) *Jobs {
	t.Helper()

	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	j, err := NewJobs(com)
	require.NoError(t, err)
	return j
}

// TestJobs_EnterAsksBeforeKilling pins that enter does not terminate a job
// outright. Killing a background job is destructive and irreversible — the
// dialog's own ctrl+x path asks "Kill this job?" first — yet enter used to
// return ActionKillJob straight from the key handler while the help text
// still advertised it as "choose". A user arrowing through the list and
// pressing enter to look at a job would kill it instead.
func TestJobs_EnterAsksBeforeKilling(t *testing.T) {
	ids := startTestJobs(t, 2)

	j := newTestJobs(t)
	require.Equal(t, len(ids), j.list.Len())
	j.list.SetSelected(1)

	action := j.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.NotEqual(t, ActionKillJob{ShellID: ids[1]}, action,
		"enter must not kill the highlighted job without confirmation")
	require.Nil(t, action, "enter must only arm the confirmation, not act")
	require.Equal(t, jobsModeKilling, j.mode, "enter must arm the kill confirmation")
}

// TestJobs_ConfirmKillTargetsHighlightedJob covers the other direction: the
// confirmation must still kill, and it must kill the job the user
// highlighted. Entering kill mode calls refresh(), which rebuilds the list
// from BackgroundShellManager.ListJobs while list.SetItems keeps the selected
// *index* — so this also pins that ListJobs does not reshuffle underneath the
// cursor.
func TestJobs_ConfirmKillTargetsHighlightedJob(t *testing.T) {
	ids := startTestJobs(t, 6)

	j := newTestJobs(t)
	require.Equal(t, len(ids), j.list.Len())
	j.list.SetSelected(4)

	require.Nil(t, j.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	action := j.HandleMsg(tea.KeyPressMsg{Code: 'y', Text: "y"})

	require.Equal(t, ActionKillJob{ShellID: ids[4]}, action,
		"confirming must kill the highlighted job, not another one")
	require.Equal(t, jobsModeNormal, j.mode)
}

// TestJobs_CancelKillDoesNotKill pins that answering "n" leaves every job
// alone.
func TestJobs_CancelKillDoesNotKill(t *testing.T) {
	startTestJobs(t, 2)

	j := newTestJobs(t)
	j.list.SetSelected(0)

	require.Nil(t, j.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, jobsModeKilling, j.mode)
	action := j.HandleMsg(tea.KeyPressMsg{Code: 'n', Text: "n"})

	require.Nil(t, action, "cancelling must not produce a kill action")
	require.Equal(t, jobsModeNormal, j.mode)
}
