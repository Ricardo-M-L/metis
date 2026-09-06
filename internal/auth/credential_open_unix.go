//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package auth

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openCredentialStoreFile opens the final path component without following a
// symbolic link, then validates and secures the opened inode itself. The
// linked-path comparison rejects an atomic replacement that races the open.
func openCredentialStoreFile(path string, maxSize int64, warnLoose bool) (*os.File, bool, error) {
	// Nonblocking open lets fstat reject a FIFO without waiting for a writer.
	// It has no effect on regular-file reads and preserves O_NOFOLLOW safety.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, false, errors.New("refusing symlink credential store")
		}
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("open credential store: invalid file descriptor")
	}
	if err := validateOpenedCredentialStore(path, file, maxSize, warnLoose); err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	return file, true, nil
}

func validateOpenedCredentialStore(path string, file *os.File, maxSize int64, warnLoose bool) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return errors.New("credential store is not a regular file")
	}
	if opened.Size() > maxSize {
		return errors.New("credential store is too large")
	}
	mode := opened.Mode().Perm()
	if warnLoose && mode&^os.FileMode(0o600) != 0 {
		fmt.Fprintf(os.Stderr, "metis: %s has loose perms %#o; tightening to 0600\n", path, mode)
	}
	// Tighten permissions (including Darwin ACLs) on the opened inode, so a
	// concurrent pathname replacement cannot redirect this change.
	if err := secureOpenedOAuthStoreFile(file); err != nil {
		return fmt.Errorf("secure credential store for current user: %w", err)
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("credential store changed while opening: %w", err)
	}
	if linked.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink credential store")
	}
	if !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.New("credential store changed while opening")
	}
	return nil
}
