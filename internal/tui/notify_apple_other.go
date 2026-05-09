//go:build !darwin

package tui

// Stub for non-darwin builds. Apple Terminal only exists on macOS, so
// the probe always reports "bell on" → notification suppressed. The
// auto-channel path on Linux/Windows hits the KITTY_WINDOW_ID /
// ALACRITTY_LOG / TERM_PROGRAM branches instead, never falling here.
func isAppleTerminalAudibleBellDisabled() bool { return false }
