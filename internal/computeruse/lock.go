package computeruse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

func (m *Manager) lock(ctx context.Context) (func(), string, error) {
	directory, err := m.directory()
	if err != nil {
		return nil, "", err
	}
	if err := checkManagedDirectories(directory, true); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	lockPath := filepath.Join(directory, ".install-lock")
	// Never unlink this file, including on release. Unlinking would allow a new
	// installer to lock a different inode while an existing waiter owns this one.
	file, err := openInstallLock(lockPath)
	if err != nil {
		return nil, "", fmt.Errorf("open computer-use install lock %s: %w", lockPath, err)
	}
	locked := false
	defer func() {
		if !locked {
			_ = file.Close()
		}
	}()
	if err := validateInstallLock(file, lockPath); err != nil {
		return nil, "", err
	}
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", fmt.Errorf("waiting for computer-use install lock %s: %w", lockPath, err)
		}
		acquired, err := tryInstallLock(file)
		if err != nil {
			return nil, "", fmt.Errorf("lock computer-use installer: %w", err)
		}
		if acquired {
			if err := validateInstallLock(file, lockPath); err != nil {
				return nil, "", err
			}
			locked = true
			var once sync.Once
			// Closing the descriptor releases ownership. The OS also closes it on
			// process termination, including crashes and forced termination.
			return func() { once.Do(func() { _ = file.Close() }) }, directory, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func validateInstallLock(file *os.File, filename string) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("computer-use install lock must be the same regular file, not a symlink or directory")
	}
	// Windows uses ACLs inherited from the private component directory instead
	// of Unix mode bits; FileInfo reports 0666 even when that ACL is private.
	if runtime.GOOS != "windows" && opened.Mode().Perm() != 0600 {
		return errors.New("computer-use install lock must have private permissions 0600")
	}
	return nil
}
