//go:build linux

package model

import (
	"os"
	"os/exec"
)

// waylandImageTypes are the clipboard MIME types we try, in order of
// preference, when reading an image off the Wayland clipboard.
var waylandImageTypes = []string{"image/png", "image/jpeg"}

// readWaylandImage reads image bytes from the Wayland clipboard via
// wl-paste. The native clipboard reader only talks to X11 (through
// XWayland), so on a Wayland session it usually can't see an image that
// was copied to the Wayland clipboard (e.g. by grim/hyprshot or a
// browser). This fallback closes that gap.
//
// It returns ok=false when the session is not Wayland, when wl-paste is
// not installed, or when the clipboard holds no image — leaving the
// caller to fall back to the native (X11) read.
func readWaylandImage() ([]byte, bool) {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, false
	}
	for _, mime := range waylandImageTypes {
		// --no-newline keeps wl-paste from appending a trailing byte to
		// the raw image data.
		out, err := exec.Command("wl-paste", "--no-newline", "--type", mime).Output()
		if err == nil && len(out) > 0 {
			return out, true
		}
	}
	return nil, false
}
