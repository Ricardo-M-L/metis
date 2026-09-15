//go:build windows

package computeruse

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func openInstallLock(filename string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(filename)
	if err != nil {
		return nil, err
	}
	// OPEN_REPARSE_POINT opens the link itself for validation instead of its
	// target. Omitting FILE_SHARE_DELETE keeps the inode stable while open.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("computer-use install lock must not be a reparse point or symlink")
	}
	return os.NewFile(uintptr(handle), filename), nil
}

func tryInstallLock(file *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}
