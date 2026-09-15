//go:build !darwin && !linux

package computeruse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestProbeUnsupportedSandboxFailsClosed(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "unused-helper.exe")
	if err := os.WriteFile(filename, []byte("not an executable"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := probeWithHome(context.Background(), filename, t.TempDir())
	if !errors.Is(err, sandbox.ErrUnsupportedPlatform) {
		t.Fatalf("unsupported platform tried to execute a probe: %v", err)
	}
}
