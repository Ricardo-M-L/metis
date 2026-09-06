//go:build !windows

package auth

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockOAuthStoreFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return false, err
}

func unlockOAuthStoreFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }

func syncOAuthStoreDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open credential-store directory for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync credential-store directory: %w", err)
	}
	return file.Close()
}

func replaceOAuthStoreFile(source, destination string) error { return os.Rename(source, destination) }
