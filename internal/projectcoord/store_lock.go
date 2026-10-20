package projectcoord

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const projectCoordinatorLockRetry = 10 * time.Millisecond

type projectCoordinatorFileLock struct {
	file *os.File
	once sync.Once
}

func ensureProjectCoordinatorLockDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create project coordinator lock directory: %w", err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return errors.New("refusing symlinked or non-directory project coordinator lock directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, after) {
		return errors.New("project coordinator lock directory changed while securing")
	}
	return nil
}

// The lock path remains the same as older releases, but the inode is now a
// persistent regular file. Never unlink it: another process can be waiting on
// the same inode, and replacing it would create two independent locks.
func acquireProjectCoordinatorLock(path string, timeout time.Duration) (*projectCoordinatorFileLock, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("legacy project coordinator lock directory %q exists; stop old coordinator workers and remove the legacy lock directory before restarting; it will not be removed automatically", path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("refusing symlinked or non-regular project coordinator lock")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := openProjectCoordinatorLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("open project coordinator lock: %w", err)
	}
	closeWith := func(lockErr error) (*projectCoordinatorFileLock, error) {
		return nil, errors.Join(lockErr, file.Close())
	}
	if err := verifyProjectCoordinatorLockFile(path, file); err != nil {
		return closeWith(err)
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWith(fmt.Errorf("secure project coordinator lock: %w", err))
	}
	deadline := time.Now().Add(timeout)
	for {
		locked, err := tryLockProjectCoordinatorFile(file)
		if err != nil {
			return closeWith(fmt.Errorf("acquire project coordinator lock: %w", err))
		}
		if locked {
			if err := verifyProjectCoordinatorLockFile(path, file); err != nil {
				_ = unlockProjectCoordinatorFile(file)
				return closeWith(err)
			}
			return &projectCoordinatorFileLock{file: file}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return closeWith(fmt.Errorf("timed out waiting for project coordinator lock after %s", timeout))
		}
		time.Sleep(min(remaining, projectCoordinatorLockRetry))
	}
}

func verifyProjectCoordinatorLockFile(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.New("refusing symlinked, non-regular, or replaced project coordinator lock")
	}
	return nil
}

func (lock *projectCoordinatorFileLock) release() {
	lock.once.Do(func() {
		_ = unlockProjectCoordinatorFile(lock.file)
		_ = lock.file.Close()
	})
}
