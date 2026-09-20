//go:build windows

package agent

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func openCronRunLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("cron runs: unsafe lock file")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func tryLockCronRunFile(f *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
