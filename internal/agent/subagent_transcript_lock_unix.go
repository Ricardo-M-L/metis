//go:build !windows

package agent

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func trySubAgentTranscriptLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrSubAgentTranscriptBusy
	}
	if err != nil {
		return fmt.Errorf("lock subagent transcript producer: %w", err)
	}
	return nil
}

func syncSubAgentDirectoryFile(f *os.File) error { return f.Sync() }
