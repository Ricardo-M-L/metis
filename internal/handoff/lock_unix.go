//go:build !windows

package handoff

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func openLock(p string) (*os.File, error) {
	fd, err := unix.Open(p, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}
func openRecord(p string) (*os.File, error) {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return false, err
}
func unlock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func syncDirectory(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
