package artifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const artifactWriteLockFilename = ".store.lock"

// acquireWriteLock serializes a full read-modify-publish transaction across
// independent CLI and Desktop worker processes. The stable lock inode is never
// removed or replaced, including when artifacts or sessions are deleted.
// Callers also hold s.mu so same-process readers keep their existing behavior.
func (s *Store) acquireWriteLock() (*os.File, error) {
	if err := requirePrivateDirectory(s.root); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, artifactWriteLockFilename)
	before, err := os.Lstat(path)
	if err == nil {
		if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || !hasPrivatePermissions(before.Mode(), 0o600) {
			return nil, fmt.Errorf("%w: store lock must be a private regular file", ErrUnsafeFile)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("artifact: inspect store lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("artifact: open store lock: %w", err)
	}
	closeWith := func(lockErr error) (*os.File, error) {
		return nil, errors.Join(lockErr, file.Close())
	}
	if err := verifyArtifactWriteLock(path, file, before); err != nil {
		return closeWith(err)
	}
	if err := lockArtifactStoreFile(file); err != nil {
		return closeWith(fmt.Errorf("artifact: acquire store lock: %w", err))
	}
	// A process may have waited while a lock path was replaced. Never proceed
	// with an exclusive lock on an inode other writers would no longer use.
	if err := verifyArtifactWriteLock(path, file, before); err != nil {
		return nil, errors.Join(err, releaseArtifactWriteLock(file))
	}
	return file, nil
}

func verifyArtifactWriteLock(path string, file *os.File, before os.FileInfo) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("artifact: inspect opened store lock: %w", err)
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: store lock disappeared", ErrUnsafeFile)
	}
	if !opened.Mode().IsRegular() || !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 ||
		!hasPrivatePermissions(opened.Mode(), 0o600) || !hasPrivatePermissions(linked.Mode(), 0o600) ||
		!os.SameFile(opened, linked) || (before != nil && !os.SameFile(before, opened)) {
		return fmt.Errorf("%w: store lock is non-private, non-regular or replaced", ErrUnsafeFile)
	}
	return nil
}

func releaseArtifactWriteLock(file *os.File) error {
	return errors.Join(unlockArtifactStoreFile(file), file.Close())
}
