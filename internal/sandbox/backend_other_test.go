//go:build !darwin && !linux

package sandbox

import (
	"errors"
	"os/exec"
	"testing"
)

func TestUnsupportedPlatformFailsClosedWhenEnabled(t *testing.T) {
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	_, err = m.Wrap(exec.Command("program"), Request{Cwd: t.TempDir()})
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Wrap error = %v, want ErrUnsupportedPlatform", err)
	}
}
