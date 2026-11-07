//go:build !windows

package tasks

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockTaskFile(f *os.File) error   { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }
func unlockTaskFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
