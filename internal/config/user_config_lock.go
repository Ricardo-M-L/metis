package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

const (
	userConfigLockFilename = ".user-config.lock"
	userConfigLockTimeout  = 2 * time.Second
	userConfigLockRetry    = 10 * time.Millisecond
)

var (
	userConfigWriteMu        sync.Mutex
	errUserConfigLockTimeout = errors.New("timed out waiting for user config lock")
)

type userConfigFileLock struct {
	file *os.File
}

// withUserConfigWriteLock serializes the complete read-modify-replace
// transaction both within this process and across concurrently running Metis
// processes. Callers must not nest it; public config writers enter through
// this single helper exactly once.
func withUserConfigWriteLock(fn func(configPath string) error) error {
	return withUserConfigWriteLockTimeout(userConfigLockTimeout, fn)
}

func withUserConfigWriteLockTimeout(timeout time.Duration, fn func(configPath string) error) (err error) {
	userConfigWriteMu.Lock()
	defer userConfigWriteMu.Unlock()

	dir, err := auth.EnsureCredentialHome("")
	if err != nil {
		return fmt.Errorf("resolve metis home for user config: %w", err)
	}
	lock, err := acquireUserConfigFileLock(dir, timeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release user config lock: %w", releaseErr))
		}
	}()
	return fn(filepath.Join(dir, "config.toml"))
}

func acquireUserConfigFileLock(dir string, timeout time.Duration) (*userConfigFileLock, error) {
	path := filepath.Join(dir, userConfigLockFilename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open user config lock: %w", err)
	}
	closeWith := func(lockErr error) (*userConfigFileLock, error) {
		return nil, errors.Join(lockErr, file.Close())
	}
	if err := verifyUserConfigLockFile(path, file); err != nil {
		return closeWith(err)
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWith(fmt.Errorf("set user config lock permissions: %w", err))
	}

	deadline := time.Now().Add(timeout)
	for {
		locked, err := tryLockUserConfigFile(file)
		if err != nil {
			return closeWith(fmt.Errorf("acquire user config lock: %w", err))
		}
		if locked {
			return &userConfigFileLock{file: file}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return closeWith(fmt.Errorf("%w after %s", errUserConfigLockTimeout, timeout))
		}
		if remaining > userConfigLockRetry {
			remaining = userConfigLockRetry
		}
		time.Sleep(remaining)
	}
}

func verifyUserConfigLockFile(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened user config lock: %w", err)
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect user config lock: %w", err)
	}
	if linked.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlinked user config lock")
	}
	if !opened.Mode().IsRegular() || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.New("refusing non-regular or replaced user config lock")
	}
	return nil
}

func (lock *userConfigFileLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	return errors.Join(unlockUserConfigFile(lock.file), lock.file.Close())
}
