//go:build windows

package principal

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func openLock(p string) (*os.File, error) {
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("job lock must be a regular file")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
}
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
func unlock(f *os.File) {
	var o windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &o)
}
