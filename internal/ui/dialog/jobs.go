package dialog

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/util"
	uv "github.com/charmbracelet/ultraviolet"
)

// JobsID is the identifier for the background jobs dialog.
const JobsID = "jobs"

type jobsMode uint8

const (
	jobsModeNormal jobsMode = iota
	jobsModeKilling
)

// Jobs is a background jobs list dialog.
type Jobs struct {
	com   *common.Common
	help  help.Model
	list  *list.FilterableList
	input textinput.Model

	mode jobsMode

	jobs []*shell.BackgroundShell

	keyMap struct {
		Kill        key.Binding
		ConfirmKill key.Binding
		CancelKill  key.Binding
		Next        key.Binding
		Previous    key.Binding
		UpDown      key.Binding
		Select      key.Binding
		Close       key.Binding
	}
}

var _ Dialog = (*Jobs)(nil)

// NewJobs creates a new Jobs dialog.
func NewJobs(com *common.Common) (*Jobs, error) {
	j := &Jobs{
		com:  com,
		mode: jobsModeNormal,
	}

	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()
	j.help = help

	j.list = list.NewFilterableList()
	j.list.Focus()
	j.list.SetSelected(0)

	j.input = textinput.New()
	j.input.SetVirtualCursor(false)
	j.input.Placeholder = "Type to filter"
	j.input.SetStyles(com.Styles.TextInput)
	j.input.Focus()

	j.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "kill"),
	)
	j.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	j.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	j.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑↓", "choose"),
	)
	j.keyMap.Kill = key.NewBinding(
		key.WithKeys("ctrl+x"),
		key.WithHelp("ctrl+x", "kill"),
	)
	j.keyMap.ConfirmKill = key.NewBinding(
		key.WithKeys("y"),
		key.WithHelp("y", "kill"),
	)
	j.keyMap.CancelKill = key.NewBinding(
		key.WithKeys("n", "esc"),
		key.WithHelp("n", "cancel"),
	)
	j.keyMap.Close = CloseKey

	j.refresh()
	return j, nil
}

// ID implements Dialog.
func (j *Jobs) ID() string {
	return JobsID
}

// HandleMsg implements Dialog.
func (j *Jobs) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch j.mode {
		case jobsModeKilling:
			switch {
			case key.Matches(msg, j.keyMap.ConfirmKill):
				action := j.confirmKill()
				j.mode = jobsModeNormal
				j.refresh()
				return action
			case key.Matches(msg, j.keyMap.CancelKill):
				j.mode = jobsModeNormal
				j.refresh()
			}
		default:
			switch {
			case key.Matches(msg, j.keyMap.Close):
				return ActionClose{}
			case key.Matches(msg, j.keyMap.Kill):
				if j.selectedJob() == nil {
					return ActionCmd{Cmd: util.ReportWarn("No job selected")}
				}
				j.mode = jobsModeKilling
				j.refresh()
			case key.Matches(msg, j.keyMap.Previous):
				j.list.Focus()
				if j.list.IsSelectedFirst() {
					j.list.SelectLast()
				} else {
					j.list.SelectPrev()
				}
				j.list.ScrollToSelected()
			case key.Matches(msg, j.keyMap.Next):
				j.list.Focus()
				if j.list.IsSelectedLast() {
					j.list.SelectFirst()
				} else {
					j.list.SelectNext()
				}
				j.list.ScrollToSelected()
			case key.Matches(msg, j.keyMap.Select):
				// Killing a job is destructive and irreversible, so enter
				// asks first exactly like ctrl+x rather than terminating
				// the highlighted job outright.
				if j.selectedJob() == nil {
					return ActionCmd{Cmd: util.ReportWarn("No job selected")}
				}
				j.mode = jobsModeKilling
				j.refresh()
			default:
				var cmd tea.Cmd
				j.input, cmd = j.input.Update(msg)
				j.list.SetFilter(j.input.Value())
				j.list.ScrollToTop()
				j.list.SetSelected(0)
				return ActionCmd{Cmd: cmd}
			}
		}
	}
	return nil
}

// Cursor returns the cursor position relative to the dialog.
func (j *Jobs) Cursor() *tea.Cursor {
	return InputCursor(j.com.Styles, j.input.Cursor())
}

// Draw implements Dialog.
func (j *Jobs) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := j.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	j.input.SetWidth(dialogInputTextWidth(t, j.input, innerWidth))
	listHeight, listTotalHeight, listWidth := sizeDialogList(t, j.list, innerWidth, height)

	// Hide the info column uniformly when the widest would crowd the title.
	applyInfoColumnVisibility(j.list.FilteredItems(), listWidth, sessionInfoMaxPercent)

	// Scroll to selected if outside visible range.
	start, end := j.list.VisibleItemIndices()
	if idx := j.list.Selected(); idx < start || idx > end {
		j.list.ScrollToSelected()
	}

	var cur *tea.Cursor
	rc := NewRenderContext(t, width)
	rc.Title = "Background Jobs"

	switch j.mode {
	case jobsModeKilling:
		rc.TitleStyle = t.Dialog.Sessions.DeletingTitle
		rc.TitleGradientFromColor = t.Dialog.Sessions.DeletingTitleGradientFromColor
		rc.TitleGradientToColor = t.Dialog.Sessions.DeletingTitleGradientToColor
		rc.ViewStyle = t.Dialog.Sessions.DeletingView
		rc.AddPart(t.Dialog.Sessions.DeletingMessage.Render("Kill this job?"))
	default:
		inputView := t.Dialog.InputPrompt.Render(j.input.View())
		cur = j.Cursor()
		rc.AddPart(inputView)
	}

	listView := t.Dialog.List.Height(j.list.Height()).Render(j.list.Render())
	listView = joinScrollbar(t, listView, listHeight, listTotalHeight, listHeight, j.list.Offset())
	rc.AddPart(listView)
	rc.Help = renderDialogHelp(t, &j.help, j, innerWidth)

	view := rc.Render()

	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// refresh reloads the background shell list and rebuilds list items.
func (j *Jobs) refresh() {
	manager := shell.GetBackgroundShellManager()
	j.jobs = manager.ListJobs()

	items := make([]list.FilterableItem, 0, len(j.jobs))
	for _, job := range j.jobs {
		items = append(items, NewJobItem(j.com.Styles, job, j.mode))
	}
	j.list.SetItems(items...)
}

func (j *Jobs) selectedJob() *shell.BackgroundShell {
	if item := j.list.SelectedItem(); item != nil {
		if ji, ok := item.(*JobItem); ok {
			return ji.job
		}
	}
	return nil
}

func (j *Jobs) confirmKill() Action {
	job := j.selectedJob()
	if job == nil {
		return ActionCmd{Cmd: util.ReportWarn("No job selected")}
	}
	return ActionKillJob{ShellID: job.ID}
}

// ShortHelp implements [help.KeyMap].
func (j *Jobs) ShortHelp() []key.Binding {
	switch j.mode {
	case jobsModeKilling:
		return []key.Binding{
			j.keyMap.ConfirmKill,
			j.keyMap.CancelKill,
		}
	default:
		return []key.Binding{
			j.keyMap.UpDown,
			j.keyMap.Kill,
			j.keyMap.Select,
			j.keyMap.Close,
		}
	}
}

// FullHelp implements [help.KeyMap].
func (j *Jobs) FullHelp() [][]key.Binding {
	m := [][]key.Binding{}
	slice := []key.Binding{
		j.keyMap.UpDown,
		j.keyMap.Kill,
		j.keyMap.Select,
		j.keyMap.Close,
	}

	switch j.mode {
	case jobsModeKilling:
		slice = []key.Binding{
			j.keyMap.ConfirmKill,
			j.keyMap.CancelKill,
		}
	}
	for i := 0; i < len(slice); i += 4 {
		end := min(i+4, len(slice))
		m = append(m, slice[i:end])
	}
	return m
}

// InitialCmd returns the initial command to focus the input.
func (j *Jobs) InitialCmd() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, j.input.Focus())
	return tea.Batch(cmds...)
}
