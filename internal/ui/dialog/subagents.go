package dialog

import (
	"context"
	"fmt"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// SubagentsID is the identifier for the subagents dialog.
const SubagentsID = "subagents"

const (
	defaultSubagentsMaxWidth  = 80
	defaultSubagentsMaxHeight = 24
)

// SubagentRow is one entry in the subagents panel: either seeded from a
// persisted child session or updated live by a lifecycle event.
type SubagentRow struct {
	SessionID string
	Title     string
	Status    string // "running", "done", "error"
	Error     string
	Cost      float64
	StartedAt time.Time
}

// Subagents is a dialog listing all subagents of the current session,
// seeded from ListChildren and overlaid with live lifecycle events.
type Subagents struct {
	com        *common.Common
	sessionID  string
	listFn     func(ctx context.Context, sessionID string) ([]session.Session, error)
	rows       []*SubagentRow
	rowMap     map[string]*SubagentRow
	selectedIx int

	help   help.Model
	keyMap struct {
		Close    key.Binding
		Next     key.Binding
		Previous key.Binding
	}
}

var _ Dialog = (*Subagents)(nil)

// NewSubagents creates an empty [Subagents] dialog for sessionID. Rows are
// loaded via loadFn (usually Workspace.ListChildren).
func NewSubagents(com *common.Common, sessionID string, loadFn func(ctx context.Context, sessionID string) ([]session.Session, error)) *Subagents {
	s := &Subagents{
		com:       com,
		sessionID: sessionID,
		listFn:    loadFn,
		rowMap:    make(map[string]*SubagentRow),
	}

	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()
	s.help = help

	s.keyMap.Close = key.NewBinding(
		key.WithKeys("esc", "q"),
		key.WithHelp("esc/q", "close"),
	)
	s.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "next"),
	)
	s.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "prev"),
	)

	return s
}

// ID implements [Dialog].
func (s *Subagents) ID() string {
	return SubagentsID
}

// SessionID returns the parent session this panel tracks.
func (s *Subagents) SessionID() string {
	return s.sessionID
}

// Load seeds rows from the persisted child sessions. Existing live state
// for a session ID wins over the persisted snapshot.
func (s *Subagents) Load(ctx context.Context) {
	if s.listFn == nil || s.sessionID == "" {
		return
	}
	children, err := s.listFn(ctx, s.sessionID)
	if err != nil {
		return
	}
	for _, child := range children {
		if _, exists := s.rowMap[child.ID]; exists {
			continue
		}
		s.upsert(&SubagentRow{
			SessionID: child.ID,
			Title:     child.Title,
			Status:    "done",
			Cost:      child.Cost,
			StartedAt: time.Unix(child.CreatedAt, 0),
		})
	}
}

// HandleLifecycle applies a TypeSubAgentLifecycle event as an idempotent
// upsert keyed by SubSessionID.
func (s *Subagents) HandleLifecycle(ev *notify.SubAgentLifecycle) {
	if ev == nil || ev.SubSessionID == "" {
		return
	}
	if ev.ParentSessionID != "" && s.sessionID != "" && ev.ParentSessionID != s.sessionID {
		return
	}

	status := ev.Phase
	switch status {
	case "start":
		status = "running"
	case "done":
		status = "done"
	case "error":
		status = "error"
	default:
		return
	}

	row := s.rowMap[ev.SubSessionID]
	if row == nil {
		row = &SubagentRow{
			SessionID: ev.SubSessionID,
			StartedAt: time.Now(),
		}
	}
	if ev.Title != "" {
		row.Title = ev.Title
	}
	if row.Title == "" {
		row.Title = ev.SubSessionID
	}
	row.Status = status
	if status == "error" {
		row.Error = ev.Error
	}
	s.upsert(row)
}

func (s *Subagents) upsert(row *SubagentRow) {
	if existing, ok := s.rowMap[row.SessionID]; ok {
		*existing = *row
		return
	}
	s.rows = append(s.rows, row)
	s.rowMap[row.SessionID] = row
}

// Rows exposes the current rows (newest last) for tests and rendering.
func (s *Subagents) Rows() []*SubagentRow {
	return s.rows
}

// HandleMsg implements [Dialog].
func (s *Subagents) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, s.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, s.keyMap.Next):
			if s.selectedIx < len(s.rows)-1 {
				s.selectedIx++
			}
		case key.Matches(msg, s.keyMap.Previous):
			if s.selectedIx > 0 {
				s.selectedIx--
			}
		}
	}
	return nil
}

// ShortHelp implements [help.KeyMap].
func (s *Subagents) ShortHelp() []key.Binding {
	return []key.Binding{s.keyMap.Close, s.keyMap.Next, s.keyMap.Previous}
}

// FullHelp implements [help.KeyMap].
func (s *Subagents) FullHelp() [][]key.Binding {
	return [][]key.Binding{{s.keyMap.Close, s.keyMap.Next, s.keyMap.Previous}}
}

// Draw implements [Dialog].
func (s *Subagents) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := s.com.Styles
	width := max(0, min(defaultSubagentsMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultSubagentsMaxHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	_ = height
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()

	rc := NewRenderContext(t, width)
	rc.Title = "Sub-Agents"
	rc.TitleInfo = fmt.Sprintf("%d", len(s.rows))

	if len(s.rows) == 0 {
		rc.AddPart(t.Dialog.SecondaryText.Render("No sub-agents have run in this session yet."))
		rc.Help = renderDialogHelp(t, &s.help, s, innerWidth)
		DrawCenter(scr, area, rc.Render())
		return nil
	}

	if s.selectedIx >= len(s.rows) {
		s.selectedIx = max(0, len(s.rows)-1)
	}

	for i, row := range s.rows {
		icon := "·"
		iconStyle := t.Dialog.SecondaryText
		switch row.Status {
		case "running":
			icon = "⟳"
			iconStyle = t.Tool.TodoInProgressIcon
		case "done":
			icon = "✓"
			iconStyle = t.Tool.TodoCompletedIcon
		case "error":
			icon = "✗"
			iconStyle = t.Tool.IconError
		}

		prefix := "  "
		if i == s.selectedIx {
			prefix = "▶ "
		}

		title := row.Title
		if title == "" {
			title = row.SessionID
		}

		rowText := fmt.Sprintf("%s%s %s", prefix, iconStyle.Render(icon), title)
		if row.Cost > 0 {
			rowText += t.Dialog.SecondaryText.Render(fmt.Sprintf(" — $%.4f", row.Cost))
		}
		if row.Error != "" {
			errAvailWidth := max(10, innerWidth-lipgloss.Width(rowText)-5)
			rowText += t.Dialog.SecondaryText.Render(" — " + ansi.Truncate(row.Error, errAvailWidth, "…"))
		}

		if i == s.selectedIx {
			rc.AddPart(t.Dialog.SelectedItem.Width(innerWidth).Render(rowText))
		} else {
			rc.AddPart(t.Dialog.NormalItem.Width(innerWidth).Render(rowText))
		}
	}

	rc.Help = renderDialogHelp(t, &s.help, s, innerWidth)
	DrawCenter(scr, area, rc.Render())
	return nil
}
