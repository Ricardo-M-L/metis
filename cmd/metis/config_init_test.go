package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteStarterConfigUsesMetisHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if err := writeStarterConfig(); err != nil {
		t.Fatalf("writeStarterConfig: %v", err)
	}

	path := filepath.Join(home, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), "max_output_bytes = 32768") {
		t.Fatalf("starter config should preserve the current safe Bash output default:\n%s", b)
	}
	legacyPath := filepath.Join(xdg, "metis", "config.toml")
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("starter config unexpectedly wrote legacy path %s: %v", legacyPath, err)
	}
}

func TestWriteStarterConfigDoesNotOverwrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeStarterConfig(); err == nil {
		t.Fatal("writeStarterConfig should refuse to overwrite an existing config")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sentinel\n" {
		t.Fatalf("existing config changed: %q", b)
	}
}
