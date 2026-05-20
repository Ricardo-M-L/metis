package tui

// Unit tests for the Phase-A /mcp subcommands. We exercise the
// persistence-only branches (enable/disable/reload/edit-name-validation)
// — `test`, `logs`, and the editor-spawn part of `edit` need a live
// subprocess and aren't worth bringing into CI.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
)

// withTempMcpHome reroutes config.Home() at a fresh tmpdir so each test
// owns its mcp.toml. Mirrors withTempCuEnv but doesn't seed PATH (these
// tests don't spawn the subprocess path).
func withTempMcpHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	return home
}

// seedMCP writes a single stdio entry into mcp.toml so the enable/
// disable/edit/reload paths have something to mutate.
func seedMCP(t *testing.T, name, command string, disabled bool) {
	t.Helper()
	reg := &mcp.Registry{
		Servers: []mcp.ServerEntry{{
			Name: name, Command: command, Disabled: disabled,
		}},
	}
	if err := mcp.Save(reg); err != nil {
		t.Fatalf("seed mcp.toml: %v", err)
	}
}

func TestMCP_EnableDisable_RoundTrip(t *testing.T) {
	withTempMcpHome(t)
	seedMCP(t, "echo-server", "/bin/cat", true)

	r := (*REPL)(nil)

	out := r.handleMCPEnable("echo-server")
	if !strings.Contains(out, "enabled MCP server") {
		t.Fatalf("enable should confirm; got: %q", out)
	}

	// Verify on-disk
	reg, err := mcp.Load()
	if err != nil {
		t.Fatalf("load mcp.toml: %v", err)
	}
	entry := mcp.FindServer(reg, "echo-server")
	if entry == nil || entry.Disabled {
		t.Fatalf("expected enabled entry on disk; got: %#v", entry)
	}

	// Re-enable should be idempotent.
	out = r.handleMCPEnable("echo-server")
	if !strings.Contains(out, "already enabled") {
		t.Errorf("re-enable should be idempotent; got: %q", out)
	}

	// Disable.
	out = r.handleMCPDisable("echo-server")
	if !strings.Contains(out, "disabled MCP server") {
		t.Fatalf("disable should confirm; got: %q", out)
	}
	reg, _ = mcp.Load()
	if !mcp.FindServer(reg, "echo-server").Disabled {
		t.Fatalf("expected disabled=true on disk")
	}
}

func TestMCP_EnableUnknown(t *testing.T) {
	withTempMcpHome(t)
	r := (*REPL)(nil)
	out := r.handleMCPEnable("ghost")
	if !strings.Contains(out, "no MCP server named") {
		t.Errorf("expected 'no MCP server named'; got: %q", out)
	}
}

func TestMCP_Reload_CountsState(t *testing.T) {
	withTempMcpHome(t)
	// Two entries — one enabled, one disabled.
	reg := &mcp.Registry{Servers: []mcp.ServerEntry{
		{Name: "live", Command: "/bin/cat"},
		{Name: "dead", Command: "/bin/cat", Disabled: true},
	}}
	if err := mcp.Save(reg); err != nil {
		t.Fatalf("save: %v", err)
	}
	r := (*REPL)(nil)
	out := r.handleMCPReload()
	if !strings.Contains(out, "1 enabled") || !strings.Contains(out, "1 disabled") {
		t.Errorf("reload should report counts; got: %q", out)
	}
}

func TestMCP_Reload_ParseError(t *testing.T) {
	home := withTempMcpHome(t)
	// Hand-write a broken mcp.toml so Load fails.
	mcpPath := filepath.Join(home, ".metis", "mcp.toml")
	_ = os.MkdirAll(filepath.Dir(mcpPath), 0o700)
	if err := os.WriteFile(mcpPath, []byte("not valid toml\n[[broken"), 0o600); err != nil {
		t.Fatalf("write broken mcp.toml: %v", err)
	}
	r := (*REPL)(nil)
	out := r.handleMCPReload()
	if !strings.Contains(out, "mcp reload:") {
		t.Errorf("expected error prefix; got: %q", out)
	}
}

func TestMCP_Edit_UnknownName(t *testing.T) {
	withTempMcpHome(t)
	seedMCP(t, "real-one", "/bin/cat", false)
	r := (*REPL)(nil)
	out := r.handleMCPEdit("typo-name")
	if !strings.Contains(out, "no MCP server named") {
		t.Errorf("expected unknown-server message; got: %q", out)
	}
}

func TestMCP_Logs_NoCaptureFile(t *testing.T) {
	withTempMcpHome(t)
	seedMCP(t, "noisy", "/bin/cat", false)
	r := (*REPL)(nil)
	out := r.handleMCPLogs("noisy")
	if !strings.Contains(out, "no captured logs") {
		t.Errorf("expected 'no captured logs' hint; got: %q", out)
	}
}

func TestMCP_Logs_UnknownServer(t *testing.T) {
	withTempMcpHome(t)
	r := (*REPL)(nil)
	out := r.handleMCPLogs("nope")
	if !strings.Contains(out, "no MCP server named") {
		t.Errorf("expected 'no MCP server named'; got: %q", out)
	}
}

func TestMCP_Test_DisabledServer(t *testing.T) {
	withTempMcpHome(t)
	seedMCP(t, "muted", "/bin/cat", true)
	r := (*REPL)(nil)
	out := r.handleMCPTest("muted")
	if !strings.Contains(out, "is disabled") {
		t.Errorf("expected 'is disabled' hint; got: %q", out)
	}
}
