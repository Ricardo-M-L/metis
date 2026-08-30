//go:build !windows

package mcp

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openRegularCacheFile(path string) (*os.File, error) {
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() {
		return nil, fmt.Errorf("cache is not a regular file")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open cache returned an invalid file")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(linked, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("cache is not a stable regular file")
	}
	return file, nil
}
