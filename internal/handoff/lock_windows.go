//go:build windows

package handoff

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func openLock(p string) (*os.File, error)   { return os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600) }
func openRecord(p string) (*os.File, error) { return os.Open(p) }
func tryLock(f *os.File) (bool, error) {
	var o windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}
func unlock(f *os.File) error {
	var o windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &o)
}
func syncDirectory(string) error { return nil }
