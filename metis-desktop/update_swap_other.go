//go:build !darwin && !linux && !windows

package main

import (
	"fmt"
	"runtime"
)

func desktopAtomicSwap(left, right string) error {
	return fmt.Errorf("atomic Desktop swap is unsupported on %s", runtime.GOOS)
}
