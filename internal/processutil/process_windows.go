//go:build windows

package processutil

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Alive checks a process handle without delivering a signal. A zero-timeout
// wait returns WAIT_TIMEOUT while the process is still running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}

// Windows has no POSIX SIGTERM through os.Process; Kill is the supported
// termination primitive for a process opened by PID.
func Terminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
