package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func desktopAtomicSwap(left, right string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, left, unix.AT_FDCWD, right, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("atomic Desktop swap: %w", err)
	}
	return nil
}
