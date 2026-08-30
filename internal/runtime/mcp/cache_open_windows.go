//go:build windows

package mcp

import (
	"fmt"
	"os"
)

func openRegularCacheFile(path string) (*os.File, error) {
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() {
		return nil, fmt.Errorf("cache is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
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
