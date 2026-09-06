//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mcpoauth

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openTokenStoreFile(path string, maxSize int64) (*os.File, bool, error) {
	// A FIFO must reach the inode type check without waiting for a writer
	// while the store's process-wide mutex is held.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, false, errors.New("refusing symlink token store")
		}
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("open token store: invalid file descriptor")
	}
	if err := validateOpenedTokenStore(path, file, maxSize); err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	return file, true, nil
}

func validateOpenedTokenStore(path string, file *os.File, maxSize int64) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return errors.New("token store is not a regular file")
	}
	if opened.Size() > maxSize {
		return errors.New("token store is too large")
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure token store for current user: %w", err)
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("token store changed while opening: %w", err)
	}
	if linked.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink token store")
	}
	if !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.New("token store changed while opening")
	}
	return nil
}
