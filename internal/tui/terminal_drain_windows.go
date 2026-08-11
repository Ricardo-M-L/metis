//go:build windows

package tui

// Windows console input is not a POSIX byte-stream file descriptor. The
// terminal reset escape sequences are still emitted, but draining requires
// console event APIs and is intentionally skipped.
func drainStdin(_ int) int { return 0 }
