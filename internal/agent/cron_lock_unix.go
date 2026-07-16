//go:build !windows

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockCronFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockCronFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
