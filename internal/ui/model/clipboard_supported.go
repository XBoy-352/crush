//go:build (linux || darwin || windows) && !arm && !386 && !ios && !android

package model

import "github.com/aymanbagabas/go-nativeclipboard"

func readClipboard(f clipboardFormat) ([]byte, error) {
	switch f {
	case clipboardFormatText:
		return nativeclipboard.Text.Read()
	case clipboardFormatImage:
		// On Wayland the native reader only sees the X11 selection (via
		// XWayland) and usually misses an image/png that lives on the
		// Wayland clipboard, so prefer wl-paste there. Falls back to the
		// native read when not on Wayland or no Wayland image is present.
		if data, ok := readWaylandImage(); ok {
			return data, nil
		}
		return nativeclipboard.Image.Read()
	}
	return nil, errClipboardUnknownFormat
}
