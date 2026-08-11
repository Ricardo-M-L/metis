//go:build !windows

package update

import (
	"errors"

	"golang.org/x/sys/unix"
)

func processAlive(pid int) (alive, known bool) {
	if pid <= 0 {
		return false, true
	}
	err := unix.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, unix.EPERM):
		return true, true
	case errors.Is(err, unix.ESRCH):
		return false, true
	default:
		return false, false
	}
}
