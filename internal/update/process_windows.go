//go:build windows

package update

import (
	"errors"

	"golang.org/x/sys/windows"
)

func processAlive(pid int) (alive, known bool) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return false, true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err == nil {
		windows.CloseHandle(h)
		return true, true
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, true
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true, true
	}
	return false, false
}
