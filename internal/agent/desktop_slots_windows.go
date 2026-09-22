//go:build windows

package agent

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// Windows does not support flock(2), so take an exclusive lock over the
// first byte of the lock file. The handle stays open for the worker's full
// sub-agent lifetime; the kernel releases it if the worker crashes.
func tryAcquireDesktopSlot(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("desktop sub-agent slots: open lock: %w", err)
	}
	handle := windows.Handle(f.Fd())
	overlapped := &windows.Overlapped{}
	err = windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err != nil {
		_ = f.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("desktop sub-agent slots: lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
			_ = f.Close()
		})
	}, true, nil
}
