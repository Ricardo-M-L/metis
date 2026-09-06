//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"syscall"
)

func rejectCredentialHardLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && stat.Nlink > 1 {
		return fmt.Errorf("sandbox: refusing credential file with multiple hard links: %q", path)
	}
	return nil
}
