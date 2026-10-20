//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func trySubAgentTranscriptLock(f *os.File) error {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrSubAgentTranscriptBusy
	}
	if err != nil {
		return fmt.Errorf("lock subagent transcript producer: %w", err)
	}
	return nil
}

// Windows does not expose directory FlushFileBuffers through os.File. File
// checkpoints still use File.Sync before an acknowledgement is issued.
func syncSubAgentDirectoryFile(*os.File) error { return nil }
