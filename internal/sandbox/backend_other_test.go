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

func TestUnsupportedPlatformRefusesBypassCredentialIsolation(t *testing.T) {
	m, err := NewManagerWithOptions(Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.RequireCredentialIsolation(true); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("RequireCredentialIsolation error = %v, want ErrUnsupportedPlatform", err)
	}
	if state := m.State(); state.CredentialIsolationRequired || state.Effective != ModeOff {
		t.Fatalf("failed isolation request changed state: %+v", state)
	}
}
