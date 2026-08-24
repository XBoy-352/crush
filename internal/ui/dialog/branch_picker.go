package dialog

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/sahilm/fuzzy"
)

const (
	// BranchPickerID is the identifier for the fork-point picker dialog.
	BranchPickerID             = "branch_picker"
	branchPickerDialogMaxWidth = 70
)

// BranchPicker is a dialog listing the current session's user messages so
// the user can pick a fork point. Selecting one emits ActionForkAtMessage;
// the original session is left intact and the UI switches to the new fork.
type BranchPicker struct {
	com   *common.Common
	help  help.Model
	list  *list.FilterableList
	input textinput.Model

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

var _ Dialog = (*BranchPicker)(nil)

// branchPickerItem implements list.FilterableItem for a user message.
type branchPickerItem struct {
	*list.Versioned
	messageID string
	preview   string
	t         *styles.Styles
	m         fuzzy.Match
	cache     map[int]string
	focused   bool
}

func (b *branchPickerItem) Finished() bool { return true }

func (b *branchPickerItem) Filter() string { return b.preview }

func (b *branchPickerItem) Render(width int) string {
	itemStyles := ListItemStyles{
		ItemBlurred:     b.t.Dialog.NormalItem,
		ItemFocused:     b.t.Dialog.SelectedItem,
		InfoTextBlurred: b.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: b.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(itemStyles, b.preview, "", b.focused, width, b.cache, &b.m)
}

func (b *branchPickerItem) SetFocused(focused bool) {
	if b.focused != focused {
		b.focused = focused
		b.cache = nil
		b.Bump()
	}
}

func (b *branchPickerItem) SetMatch(m fuzzy.Match) {
	if !sameFuzzyMatch(b.m, m) {
		b.cache = nil
		b.m = m
		b.Bump()
	}
}

// NewBranchPicker creates a new branch picker populated with the given user
// messages (newest first).
func NewBranchPicker(com *common.Common, userMessages []message.Message) *BranchPicker {
	b := &BranchPicker{com: com}

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	b.help = h

	b.list = list.NewFilterableList()
	b.list.Focus()

	b.input = textinput.New()
	b.input.SetVirtualCursor(false)
	b.input.Placeholder = "Type to filter"
	b.input.SetStyles(com.Styles.TextInput)
	b.input.Focus()

	b.keyMap.Select = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "fork here"),
	)
	b.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	b.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	b.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	b.keyMap.Close = CloseKey

	items := make([]list.FilterableItem, 0, len(userMessages))
	for _, msg := range userMessages {
		preview := strings.TrimSpace(msg.Content().Text)
		if preview == "" {
			continue
		}
		preview = strings.ReplaceAll(preview, "\n", " ")
		items = append(items, &branchPickerItem{
			Versioned: list.NewVersioned(),
			t:         com.Styles,
			messageID: msg.ID,
			preview:   preview,
		})
	}
	b.list.SetItems(items...)
	if len(items) > 0 {
		b.list.SelectFirst()
		b.list.ScrollToTop()
	}

	return b
}

// ID implements Dialog.
func (*BranchPicker) ID() string { return BranchPickerID }

// HandleMsg implements Dialog.
func (b *BranchPicker) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, b.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, b.keyMap.Previous):
			b.list.Focus()
			if b.list.IsSelectedFirst() {
				b.list.SelectLast()
				b.list.ScrollToBottom()
				break
			}
			b.list.SelectPrev()
			b.list.ScrollToSelected()
		case key.Matches(msg, b.keyMap.Next):
			b.list.Focus()
			if b.list.IsSelectedLast() {
				b.list.SelectFirst()
				b.list.ScrollToTop()
				break
			}
			b.list.SelectNext()
			b.list.ScrollToSelected()
		case key.Matches(msg, b.keyMap.Select):
			selectedItem := b.list.SelectedItem()
			if selectedItem == nil {
				break
			}
			item, ok := selectedItem.(*branchPickerItem)
			if !ok {
				break
			}
			return ActionForkAtMessage{MessageID: item.messageID}
		default:
			var cmd tea.Cmd
			b.input, cmd = b.input.Update(msg)
			b.list.SetFilter(b.input.Value())
			b.list.ScrollToTop()
			b.list.SetSelected(0)
			return ActionCmd{cmd}
		}
	}
	return nil
}

// ShortHelp implements [help.KeyMap].
func (b *BranchPicker) ShortHelp() []key.Binding {
	return []key.Binding{b.keyMap.UpDown, b.keyMap.Select, b.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (b *BranchPicker) FullHelp() [][]key.Binding {
	return [][]key.Binding{{b.keyMap.Select, b.keyMap.Next, b.keyMap.Previous, b.keyMap.Close}}
}

// Cursor returns the cursor position relative to the dialog.
func (b *BranchPicker) Cursor() *tea.Cursor {
	return InputCursor(b.com.Styles, b.input.Cursor())
}

// Draw implements Dialog.
func (b *BranchPicker) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := b.com.Styles
	width := max(0, min(branchPickerDialogMaxWidth, area.Dx()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	b.input.SetWidth(innerWidth - t.Dialog.InputPrompt.GetHorizontalFrameSize() - 1)
	visibleCount := b.list.Len()
	listHeight := max(3, min(visibleCount, area.Dy()-heightOffset))
	b.list.SetSize(innerWidth, listHeight)
	b.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	rc.Title = "Fork From Message"
	inputView := t.Dialog.InputPrompt.Render(b.input.View())
	rc.AddPart(inputView)

	if b.list.Height() >= visibleCount {
		b.list.ScrollToTop()
	} else {
		b.list.ScrollToSelected()
	}

	listView := t.Dialog.List.Height(b.list.Height()).Render(b.list.Render())
	rc.AddPart(listView)
	rc.Help = b.help.View(b)

	view := rc.Render()
	cur := b.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}
