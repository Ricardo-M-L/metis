//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package computeruse

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openInstallLock(filename string) (*os.File, error) {
	// NOFOLLOW rejects symlinks atomically; NONBLOCK avoids hanging on a FIFO
	// before the common regular-file check can reject it.
	fd, err := unix.Open(filename, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filename), nil
}

func tryInstallLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return false, err
}
