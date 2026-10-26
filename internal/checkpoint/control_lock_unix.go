//go:build !windows

package checkpoint

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func tryControlLock(file *os.File, exclusive bool) (bool, error) {
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return false, err
}
func unlockControl(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
