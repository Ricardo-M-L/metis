//go:build !windows

package memory

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockRepositoryFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockRepositoryFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
