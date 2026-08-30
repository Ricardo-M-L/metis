//go:build !windows

package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLoadCacheRejectsNonRegularFile(t *testing.T) {
	home := setMetisHome(t)
	dir := filepath.Join(home, "mcp-cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := CachePath("fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	// A writer keeps the pre-fix implementation from hanging the test forever;
	// the hardened loader must reject the FIFO before consuming any bytes.
	writerDone := make(chan struct{})
	stopWriter := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err == nil {
				file := os.NewFile(uintptr(fd), path)
				_, _ = file.WriteString(`{"version":3,"fingerprint":"fifo"}`)
				_ = file.Close()
				return
			}
			select {
			case <-stopWriter:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	_, err := LoadCache("fifo")
	close(stopWriter)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "regular") {
		t.Fatalf("LoadCache FIFO error = %v, want non-regular rejection", err)
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("FIFO writer helper did not exit")
	}
}
