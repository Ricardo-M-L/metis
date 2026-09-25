//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Inherited stdin is a blocking descriptor outside Go's poller. Closing that
// os.File does not reliably interrupt a pending Read. Duplicate it and mark it
// nonblocking before NewFile so the runtime can wake the reader on Close.
func desktopWorkerInput() (io.ReadCloser, error) {
	fd, err := unix.Dup(int(os.Stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate desktop worker stdin: %w", err)
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "desktop-worker-replies"), nil
}
