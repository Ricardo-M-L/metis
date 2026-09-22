//go:build unix

package agent

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

func tryAcquireDesktopSlot(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("desktop sub-agent slots: open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("desktop sub-agent slots: lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
		})
	}, true, nil
}
