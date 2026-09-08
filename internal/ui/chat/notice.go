package chat

import (
	"strings"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// NoticeMessageItem renders a machine-generated notification (background
// job or subagent completion) as a dimmed system line in the transcript.
// It is visually distinct from a human chat bubble: no border, no user
// marker, subdued foreground.
type NoticeMessageItem struct {
	*list.Versioned
	message *message.Message
	sty     *styles.Styles
}

// NewNoticeMessageItem creates a new NoticeMessageItem.
func NewNoticeMessageItem(sty *styles.Styles, message *message.Message) *NoticeMessageItem {
	return &NoticeMessageItem{
		Versioned: list.NewVersioned(),
		message:   message,
		sty:       sty,
	}
}

// Finished implements list.Item. Notices are immutable once delivered.
func (m *NoticeMessageItem) Finished() bool {
	return true
}

// RawRender implements [MessageItem].
func (m *NoticeMessageItem) RawRender(width int) string {
	text := strings.TrimSpace(m.message.Content().Text)
	return m.sty.Messages.Notice.Width(width).Render(text)
}

// Render implements [MessageItem].
func (m *NoticeMessageItem) Render(width int) string {
	return m.RawRender(width)
}

// ID implements [Identifiable].
func (m *NoticeMessageItem) ID() string {
	return m.message.ID
}
