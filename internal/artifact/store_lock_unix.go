//go:build !windows

package artifact

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockArtifactStoreFile(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func unlockArtifactStoreFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
