//go:build !linux

package model

// readWaylandImage is a no-op off Linux; the Wayland clipboard fallback
// only applies to Linux sessions. See clipboard_wayland.go.
func readWaylandImage() ([]byte, bool) { return nil, false }
