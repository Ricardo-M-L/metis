package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func desktopAtomicSwap(left, right string) error {
	if err := unix.RenamexNp(left, right, unix.RENAME_SWAP); err != nil {
		return fmt.Errorf("atomic Desktop swap: %w", err)
	}
	return nil
}
