//go:build windows

package update

import "golang.org/x/sys/windows"

func renameDirNoReplace(oldPath, newPath string) error {
	oldPtr, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPtr, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	// Omitting MOVEFILE_REPLACE_EXISTING is essential: a contender must never
	// replace a fixed lock published by another installer.
	return windows.MoveFileEx(oldPtr, newPtr, windows.MOVEFILE_WRITE_THROUGH)
}
