//go:build !windows

package tui

import (
	"time"

	"golang.org/x/sys/unix"
)

// drainStdin reads and discards bytes queued before terminal input modes were
// disabled. POSIX file descriptors can be switched to nonblocking mode for a
// bounded drain; Windows console handles use a different API and no-op in the
// platform-specific companion file.
func drainStdin(fd int) int {
	time.Sleep(30 * time.Millisecond)

	if err := unix.SetNonblock(fd, true); err != nil {
		return 0
	}
	defer func() { _ = unix.SetNonblock(fd, false) }()

	buf := make([]byte, 1024)
	drained := 0
	for i := 0; i < 64; i++ {
		n, err := unix.Read(fd, buf)
		if n <= 0 || err != nil {
			return drained
		}
		drained += n
	}
	return drained
}
