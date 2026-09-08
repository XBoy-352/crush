package chat

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestExtractMessageItems_Notice(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:   "notice-1",
		Role: message.Notice,
		Parts: []message.ContentPart{
			message.TextContent{Text: "<system_reminder>job done</system_reminder>"},
		},
	}
	items := ExtractMessageItems(&sty, msg, nil, "")
	require.Len(t, items, 1)
	notice, ok := items[0].(*NoticeMessageItem)
	require.True(t, ok, "notice messages must render as NoticeMessageItem, not a user bubble")
	require.Equal(t, "notice-1", notice.ID())
	require.Contains(t, notice.RawRender(80), "job done")
}

func TestExtractMessageItems_UserIsNotNotice(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:    "user-1",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	}
	items := ExtractMessageItems(&sty, msg, nil, "")
	require.Len(t, items, 1)
	_, ok := items[0].(*UserMessageItem)
	require.True(t, ok)
}
