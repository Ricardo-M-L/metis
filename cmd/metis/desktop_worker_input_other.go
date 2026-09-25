//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"io"
	"os"
)

func desktopWorkerInput() (io.ReadCloser, error) { return os.Stdin, nil }
