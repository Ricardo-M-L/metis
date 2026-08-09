package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/version"
)

func TestRenderReleaseNotes_InstalledBinaryFallsBackToVersionedReleaseURL(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cwd := t.TempDir()
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalCWD) })
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}

	originalVersion := version.Version
	version.Version = "1.2.3"
	t.Cleanup(func() { version.Version = originalVersion })

	got := renderReleaseNotes()
	want := "https://github.com/Ricardo-M-L/metis/releases/tag/v1.2.3"
	if !strings.Contains(got, "does not include CHANGELOG.md") || !strings.Contains(got, want) {
		t.Fatalf("installed fallback = %q, want explanatory text + %s", got, want)
	}
}
