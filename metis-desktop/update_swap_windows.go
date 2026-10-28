package main

import "errors"

func desktopAtomicSwap(left, right string) error {
	return errors.New("atomic Desktop swap is unsupported on Windows")
}
