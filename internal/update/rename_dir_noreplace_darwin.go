//go:build darwin

package update

import "golang.org/x/sys/unix"

func renameDirNoReplace(oldPath, newPath string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_EXCL)
}
