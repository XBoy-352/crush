package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/stretchr/testify/require"
)

func TestPasteIdxCountsPngAndTxt(t *testing.T) {
	t.Parallel()

	t.Run("starts at 1 with no attachments", func(t *testing.T) {
		t.Parallel()
		ui := newPasteIdxTestUI()
		require.Equal(t, 1, ui.pasteIdx())
	})

	t.Run("increments past existing png pastes", func(t *testing.T) {
		t.Parallel()
		ui := newPasteIdxTestUI()
		require.True(t, ui.attachments.Update(message.Attachment{FileName: "paste_1.png"}))
		require.True(t, ui.attachments.Update(message.Attachment{FileName: "paste_2.png"}))
		require.Equal(t, 3, ui.pasteIdx())
	})

	t.Run("increments past mixed txt and png pastes", func(t *testing.T) {
		t.Parallel()
		ui := newPasteIdxTestUI()
		require.True(t, ui.attachments.Update(message.Attachment{FileName: "paste_1.txt"}))
		require.True(t, ui.attachments.Update(message.Attachment{FileName: "paste_2.png"}))
		require.Equal(t, 3, ui.pasteIdx())
	})

	t.Run("ignores unrelated attachment names", func(t *testing.T) {
		t.Parallel()
		ui := newPasteIdxTestUI()
		require.True(t, ui.attachments.Update(message.Attachment{FileName: "screenshot.png"}))
		require.Equal(t, 1, ui.pasteIdx())
	})
}

func newPasteIdxTestUI() *UI {
	return &UI{
		attachments: attachments.New(nil, attachments.Keymap{}),
	}
}
