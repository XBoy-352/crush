package model

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// jobsProbeTimeout bounds the count fetch, an HTTP round-trip in
// client/server mode.
const jobsProbeTimeout = 5 * time.Second

// jobCountsMsg delivers background-job counts fetched off-thread.
type jobCountsMsg struct {
	sessionID string
	own       int
	other     int
}

// requestJobsRefresh schedules an off-thread refresh of the memoized job
// counts, marking the state dirty while a fetch is in flight. Counts are
// event-driven, not polled: a timer would add a round-trip per tick and
// still show a stale count whenever a job finishes while the UI is idle.
func (m *UI) requestJobsRefresh() tea.Cmd {
	if m.jobsFetchInFlight {
		m.jobsRefreshQueued = true
		return nil
	}
	return m.dispatchJobsRefresh()
}

// dispatchJobsRefresh fetches the counts off the Update goroutine. The
// closure captures only locals (never m).
func (m *UI) dispatchJobsRefresh() tea.Cmd {
	if m.jobsFetchInFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	m.jobsFetchInFlight = true
	ws := m.com.Workspace
	sessionID := m.currentSessionID()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), jobsProbeTimeout)
		defer cancel()
		own, other := ws.BackgroundJobCounts(ctx, sessionID)
		return jobCountsMsg{sessionID: sessionID, own: own, other: other}
	}
}

// jobsInfo renders the sidebar's jobs section. Unconditionally, like every
// other section: one that came and went would shift the scroll position
// under the reader.
func (m *UI) jobsInfo(width int) string {
	t := m.com.Styles
	title := common.Section(t, t.Resource.Heading.Render("Background Jobs"), width)

	body := t.Resource.AdditionalText.Render("None")
	if m.runningJobsCount > 0 {
		jobWord := "jobs"
		if m.runningJobsCount == 1 {
			jobWord = "job"
		}
		body = t.Resource.StatusText.Render(fmt.Sprintf("%s %d %s running (/jobs)", styles.JobIcon, m.runningJobsCount, jobWord))
	}
	// The registry is process-wide; name other sessions' jobs as theirs.
	if m.otherSessionJobsCount > 0 {
		body += "\n" + t.Resource.AdditionalText.Render(fmt.Sprintf("%d in other sessions", m.otherSessionJobsCount))
	}

	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, body))
}

// applyJobCounts stores a result and re-dispatches if events arrived
// while it was in flight. Runs on the Update goroutine.
func (m *UI) applyJobCounts(msg jobCountsMsg) tea.Cmd {
	m.jobsFetchInFlight = false
	m.runningJobsCount = msg.own
	m.otherSessionJobsCount = msg.other
	m.jobsCountedFor = msg.sessionID
	if m.jobsRefreshQueued {
		m.jobsRefreshQueued = false
		return m.dispatchJobsRefresh()
	}
	return nil
}
