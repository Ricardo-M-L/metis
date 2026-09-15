//go:build linux

package computeruse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestProbeMissingLinuxSandboxFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	filename := isolationProbeScript(t, "printf ran > "+probeShellQuote(marker))
	_, err := Probe(context.Background(), filename)
	if !errors.Is(err, sandbox.ErrDependencyMissing) {
		t.Fatalf("missing bubblewrap did not fail closed: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unsandboxed probe executed: %v", err)
	}
}
