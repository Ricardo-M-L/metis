package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestRegistryHasEnabledServer(t *testing.T) {
	reg := &Registry{Servers: []ServerEntry{
		{Name: "disabled", Disabled: true},
		{Name: ReservedComputerUseName},
	}}
	if reg.HasEnabledServer("disabled") {
		t.Fatal("disabled server should not be reported as available")
	}
	if !reg.HasEnabledServer(ReservedComputerUseName) {
		t.Fatal("enabled computer-use server should be reported as available")
	}
	if reg.HasEnabledServer("missing") {
		t.Fatal("missing server should not be reported as available")
	}
}

// withTempHome scopes any Path() reads/writes to a fresh temp dir.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	return dir
}

func TestLoadMCP_MissingFileReturnsEmpty(t *testing.T) {
	withTempHome(t)
	r, err := Load()
	if err != nil {
		t.Fatalf("Load missing should not error: %v", err)
	}
	if len(r.Servers) != 0 {
		t.Errorf("missing file should yield empty servers; got %d", len(r.Servers))
	}
}

func TestAddSaveLoadRoundTrip(t *testing.T) {
	withTempHome(t)
	r := &Registry{}
	if err := AddServer(r, "fs", "mcp-fs", []string{"--root", "/tmp"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Servers) != 1 {
		t.Fatalf("len = %d, want 1", len(got.Servers))
	}
	s := got.Servers[0]
	if s.Name != "fs" || s.Command != "mcp-fs" {
		t.Errorf("got %+v", s)
	}
	if len(s.Args) != 2 || s.Args[0] != "--root" || s.Args[1] != "/tmp" {
		t.Errorf("args = %v", s.Args)
	}
}

func TestSaveMCP_FilePerm0600(t *testing.T) {
	withTempHome(t)
	if err := Save(&Registry{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("mcp.toml perm = %o, want 600", mode)
	}
}

func TestAdd_RejectsEmpty(t *testing.T) {
	r := &Registry{}
	if err := AddServer(r, "", "x", nil); err == nil {
		t.Error("empty name should error")
	}
	if err := AddServer(r, "x", "", nil); err == nil {
		t.Error("empty command should error")
	}
}

func TestAdd_ReplacesExisting(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "fs", "old-bin", []string{"--v1"})
	_ = AddServer(r, "fs", "new-bin", []string{"--v2"})
	if len(r.Servers) != 1 {
		t.Fatalf("re-add should replace; len = %d", len(r.Servers))
	}
	if r.Servers[0].Command != "new-bin" || r.Servers[0].Args[0] != "--v2" {
		t.Errorf("replacement didn't apply: %+v", r.Servers[0])
	}
}

func TestRemoveMCPServer(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "a", "x", nil)
	_ = AddServer(r, "b", "y", nil)

	if !RemoveServer(r, "a") {
		t.Error("Remove existing should return true")
	}
	if len(r.Servers) != 1 || r.Servers[0].Name != "b" {
		t.Errorf("after remove, expected only 'b'; got %+v", r.Servers)
	}
	if RemoveServer(r, "missing") {
		t.Error("Remove of non-existent should return false")
	}
}

func TestFindMCPServer(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "fs", "mcp-fs", nil)
	if got := FindServer(r, "fs"); got == nil || got.Name != "fs" {
		t.Errorf("Find existing failed: %+v", got)
	}
	if got := FindServer(r, "missing"); got != nil {
		t.Errorf("Find missing should return nil; got %+v", got)
	}
}

func TestMergeWithConfig_DoesNotClobberMCPToml(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "fs", "user-bin", []string{"--user"})

	cfgServers := []config.MCPServer{
		{Name: "fs", Command: "config-bin", Args: []string{"--config"}},
		{Name: "browser", Command: "browser-bin"},
	}
	r.MergeWithConfig(cfgServers)

	// fs should still point at user-bin (mcp.toml wins)
	fs := FindServer(r, "fs")
	if fs == nil || fs.Command != "user-bin" {
		t.Errorf("config clobbered runtime entry; got %+v", fs)
	}
	// browser was new — should be appended
	br := FindServer(r, "browser")
	if br == nil || br.Command != "browser-bin" {
		t.Errorf("config-only entry not merged; got %+v", br)
	}
}

func TestSavePath(t *testing.T) {
	dir := withTempHome(t)
	want := filepath.Join(dir, "mcp.toml")
	if got := Path(); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
