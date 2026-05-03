package tui

// Unit tests for /cu (computer-use) slash command. We exercise the
// pure-Go bits — mcp.toml round-trip + status reporting + PATH
// lookup — without actually spawning metis-cu (the live-load branch
// is gated on a non-nil REPL.Loop, so passing nil takes the
// persistence-only path that's safe to run in CI).

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// withTempCuEnv routes ~/.metis at a t.TempDir() and stages a fake
// metis-cu binary into a fresh PATH entry. Returns the binary's path
// for assertions; cleanup is via t.Cleanup registered in t.Setenv.
func withTempCuEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}

	binDir := filepath.Join(home, "fakebin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fakebin: %v", err)
	}
	binName := cuBinaryName
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(binDir, binName)
	// Tiny shell script body — never executed (we only LookPath it).
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho fake metis-cu\n"), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}

	// Prepend the fake-bin dir to PATH so exec.LookPath picks it up.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binPath
}

// TestCU_StatusBeforeEnable — the bare /cu (no args) case before any
// configuration. Should report "not enabled" + locate the binary.
func TestCU_StatusBeforeEnable(t *testing.T) {
	withTempCuEnv(t)
	out := cmdCU(nil, "")
	if !strings.Contains(out, "not enabled") {
		t.Errorf("status before enable should say 'not enabled'; got: %q", out)
	}
	if !strings.Contains(out, "fakebin") {
		t.Errorf("status should report the binary path under fakebin/; got: %q", out)
	}
}

// TestCU_EnableThenStatus — /cu enable persists, /cu status reflects.
func TestCU_EnableThenStatus(t *testing.T) {
	withTempCuEnv(t)

	// Enable. Pass nil REPL → persistence-only branch (no spawn).
	out := cmdCU(nil, "enable")
	if !strings.Contains(out, "enabled in mcp.toml") {
		t.Errorf("enable should confirm mcp.toml write; got: %q", out)
	}

	// Verify mcp.toml on disk.
	mcpPath := rtpkg.MCPPath()
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read mcp.toml: %v", err)
	}
	if !strings.Contains(string(data), `name = "computer-use"`) {
		t.Errorf("mcp.toml missing computer-use entry; got:\n%s", data)
	}
	if !strings.Contains(string(data), `command = "metis-cu"`) {
		t.Errorf("mcp.toml missing metis-cu command; got:\n%s", data)
	}

	// Status should now report "enabled".
	out = cmdCU(nil, "status")
	if !strings.Contains(out, "enabled") || strings.Contains(out, "not enabled") {
		t.Errorf("status after enable should be 'enabled', not 'not enabled'; got: %q", out)
	}
}

// TestCU_Disable — /cu disable removes the entry.
func TestCU_Disable(t *testing.T) {
	withTempCuEnv(t)
	cmdCU(nil, "enable")

	out := cmdCU(nil, "disable")
	if !strings.Contains(out, "disabled") {
		t.Errorf("disable should confirm; got: %q", out)
	}

	// Disabling again should report not-enabled, not error.
	out2 := cmdCU(nil, "disable")
	if !strings.Contains(out2, "not enabled") {
		t.Errorf("second disable should say 'not enabled'; got: %q", out2)
	}
}

// TestCU_EnableNoBinary — /cu enable with no metis-cu in PATH should
// surface a friendly install hint rather than silently writing the
// mcp.toml (the user would otherwise spawn a broken subprocess at
// next metis startup).
func TestCU_EnableNoBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", t.TempDir())
	}
	t.Setenv("PATH", t.TempDir()) // PATH with no metis-cu

	out := cmdCU(nil, "enable")
	if !strings.Contains(out, "not in PATH") {
		t.Errorf("enable without binary should mention 'not in PATH'; got: %q", out)
	}
	// Must NOT have written mcp.toml.
	if _, err := os.Stat(rtpkg.MCPPath()); err == nil {
		t.Errorf("enable without binary must not write mcp.toml")
	}
}

// TestCU_UnknownSubcommand — gracefully reports usage instead of
// silently no-op'ing.
func TestCU_UnknownSubcommand(t *testing.T) {
	withTempCuEnv(t)
	out := cmdCU(nil, "explode")
	if !strings.Contains(out, "unknown") {
		t.Errorf("unknown subcommand should be flagged; got: %q", out)
	}
}

// TestCU_ReEnableIdempotent — enabling twice is a no-error replace.
func TestCU_ReEnableIdempotent(t *testing.T) {
	withTempCuEnv(t)
	cmdCU(nil, "enable")
	out := cmdCU(nil, "enable")
	if !strings.Contains(out, "re-enabled") && !strings.Contains(out, "enabled") {
		t.Errorf("re-enable should succeed (re-enabled or enabled); got: %q", out)
	}
}
