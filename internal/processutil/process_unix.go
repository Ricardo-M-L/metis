//go:build !windows

package processutil

import (
	"errors"
	"syscall"
)

// Alive reports whether pid identifies a reachable process. EPERM means the
// process exists but belongs to another user, so it still counts as alive.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Terminate requests a graceful process shutdown on POSIX platforms.
func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
