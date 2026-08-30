//go:build windows

package mcpoauth

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockTokenStoreFile(file *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}

func unlockTokenStoreFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

// Syncing a directory handle is not supported by os.File on Windows. The
// token file itself is fsynced before MoveFileEx replaces the destination.
func syncTokenStoreDir(string) error { return nil }
