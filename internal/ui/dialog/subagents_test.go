package dialog

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

func newTestSubagents(t *testing.T, children []session.Session) *Subagents {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	return NewSubagents(com, "parent", func(_ context.Context, _ string) ([]session.Session, error) {
		return children, nil
	})
}

func TestSubagents_ID(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, nil)
	require.Equal(t, SubagentsID, sp.ID())
	require.Equal(t, "parent", sp.SessionID())
}

func TestSubagents_SeedFromListChildren(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, []session.Session{
		{ID: "child-1", Title: "Scout", Cost: 0.01, CreatedAt: 100},
		{ID: "child-2", Title: "Reviewer", CreatedAt: 200},
	})
	sp.Load(context.Background())

	rows := sp.Rows()
	require.Len(t, rows, 2)
	require.Equal(t, "child-1", rows[0].SessionID)
	require.Equal(t, "Scout", rows[0].Title)
	require.Equal(t, "done", rows[0].Status)
	require.InDelta(t, 0.01, rows[0].Cost, 1e-9)
	require.Equal(t, "child-2", rows[1].SessionID)
}

func TestSubagents_LoadDoesNotClobberLiveState(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, []session.Session{
		{ID: "child-1", Title: "Scout"},
	})
	// Live event arrives first: the agent is still running.
	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "parent",
		SubSessionID:    "child-1",
		Title:           "Scout",
		Phase:           "start",
	})
	sp.Load(context.Background())

	rows := sp.Rows()
	require.Len(t, rows, 1, "Load must not duplicate a live-tracked row")
	require.Equal(t, "running", rows[0].Status,
		"persisted snapshot must not overwrite live status")
}

func TestSubagents_LifecycleUpserts(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, nil)

	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "parent",
		SubSessionID:    "s1",
		Title:           "Alpha",
		Phase:           "start",
	})
	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "parent",
		SubSessionID:    "s2",
		Title:           "Beta",
		Phase:           "start",
	})
	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "parent",
		SubSessionID:    "s1",
		Title:           "Alpha",
		Phase:           "done",
	})
	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "parent",
		SubSessionID:    "s2",
		Title:           "Beta",
		Phase:           "error",
		Error:           "boom",
	})

	rows := sp.Rows()
	require.Len(t, rows, 2, "upserts must not append duplicates")
	require.Equal(t, "done", rows[0].Status)
	require.Equal(t, "error", rows[1].Status)
	require.Equal(t, "boom", rows[1].Error)
}

func TestSubagents_LifecycleFiltersOtherSessions(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, nil)
	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "some-other-session",
		SubSessionID:    "s1",
		Title:           "Foreign",
		Phase:           "start",
	})
	require.Empty(t, sp.Rows())
}

func TestSubagents_LifecycleIgnoresUnknownPhase(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, nil)
	sp.HandleLifecycle(&notify.SubAgentLifecycle{
		ParentSessionID: "parent",
		SubSessionID:    "s1",
		Phase:           "weird",
	})
	sp.HandleLifecycle(nil)
	require.Empty(t, sp.Rows())
}

func TestSubagents_KeyHandling(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		sp.HandleLifecycle(&notify.SubAgentLifecycle{
			ParentSessionID: "parent",
			SubSessionID:    id,
			Title:           id,
			Phase:           "start",
		})
	}
	require.Equal(t, 0, sp.selectedIx)

	sp.HandleMsg(tea.KeyPressMsg{Code: 'j', Text: "j"})
	require.Equal(t, 1, sp.selectedIx)
	sp.HandleMsg(tea.KeyPressMsg{Code: 'j', Text: "j"})
	sp.HandleMsg(tea.KeyPressMsg{Code: 'j', Text: "j"})
	require.Equal(t, 2, sp.selectedIx, "selection must clamp at the last row")
	sp.HandleMsg(tea.KeyPressMsg{Code: 'k', Text: "k"})
	require.Equal(t, 1, sp.selectedIx)

	action := sp.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, ActionClose{}, action)
}

func TestSubagents_Draw(t *testing.T) {
	t.Parallel()
	sp := newTestSubagents(t, []session.Session{{ID: "c1", Title: "Scout"}})
	sp.Load(context.Background())

	scr := uv.NewScreenBuffer(80, 24)
	cur := sp.Draw(scr, scr.Bounds())
	require.Nil(t, cur)

	// Empty state must also render.
	empty := newTestSubagents(t, nil)
	scr2 := uv.NewScreenBuffer(80, 24)
	cur2 := empty.Draw(scr2, scr2.Bounds())
	require.Nil(t, cur2)
}
