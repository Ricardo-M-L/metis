//go:build windows

package agent

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// Windows does not support flock(2), so take an exclusive lock over
// a byte beyond the JSON metadata. Windows byte-range locks also block reads
// and writes through other handles, so locking byte zero would prevent the
// scheduler from inspecting or updating metadata. Locking beyond EOF does not
// extend the file. The kernel releases the lock if the worker crashes.
func tryAcquireDesktopSlot(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("desktop sub-agent slots: open lock: %w", err)
	}
	handle := windows.Handle(f.Fd())
	overlapped := &windows.Overlapped{Offset: 1 << 30}
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
