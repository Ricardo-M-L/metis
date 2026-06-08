package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeLock drops a lockfile into ~/.metis/ide for the test's METIS_HOME.
func writeLock(t *testing.T, l IDELock) {
	t.Helper()
	dir := IDEDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(l)
	path := filepath.Join(dir, "ide-"+l.IDEName+".lock")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverIDE_NoDir(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if _, ok := DiscoverIDE("/some/where"); ok {
		t.Error("expected no IDE when ide dir is absent")
	}
}

func TestDiscoverIDE_WorkspaceMatch(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	ws := t.TempDir()
	writeLock(t, IDELock{
		PID: os.Getpid(), Port: 52001, Transport: "http",
		IDEName: "vscode", WorkspaceFolders: []string{ws}, AuthToken: "tok",
	})

	got, ok := DiscoverIDE(filepath.Join(ws, "pkg", "sub"))
	if !ok {
		t.Fatal("expected a match for cwd under the workspace")
	}
	if got.Port != 52001 {
		t.Errorf("port = %d, want 52001", got.Port)
	}
	if got.Endpoint() != "http://127.0.0.1:52001/mcp" {
		t.Errorf("endpoint = %q", got.Endpoint())
	}
	if h := got.AuthHeaders(); h["Authorization"] != "Bearer tok" {
		t.Errorf("auth headers = %v", h)
	}
}

func TestDiscoverIDE_StaleLockSwept(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	ws := t.TempDir()
	// PID that is essentially guaranteed not to exist.
	writeLock(t, IDELock{
		PID: 2 << 30, Port: 52002, IDEName: "dead", WorkspaceFolders: []string{ws},
	})
	if _, ok := DiscoverIDE(ws); ok {
		t.Error("dead-PID lock should not match")
	}
	// The stale lockfile should have been swept.
	entries, _ := os.ReadDir(IDEDir())
	if len(entries) != 0 {
		t.Errorf("stale lockfile not swept: %d entries remain", len(entries))
	}
}

func TestDiscoverIDE_PrefersDeepestWorkspace(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	outer := t.TempDir()
	inner := filepath.Join(outer, "nested")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLock(t, IDELock{PID: os.Getpid(), Port: 1, IDEName: "outer", WorkspaceFolders: []string{outer}})
	writeLock(t, IDELock{PID: os.Getpid(), Port: 2, IDEName: "inner", WorkspaceFolders: []string{inner}})

	got, ok := DiscoverIDE(filepath.Join(inner, "x"))
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Port != 2 {
		t.Errorf("expected the deeper (inner) workspace server (port 2), got port %d", got.Port)
	}
}

func TestDiscoverIDE_SingleServerFallback(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writeLock(t, IDELock{PID: os.Getpid(), Port: 7, IDEName: "only", WorkspaceFolders: []string{"/elsewhere"}})
	// cwd is outside any workspace, but a single live server is the
	// obvious target.
	got, ok := DiscoverIDE("/tmp/unrelated")
	if !ok || got.Port != 7 {
		t.Errorf("expected single-server fallback to port 7, got ok=%v port=%v", ok, portOf(got))
	}
}

func TestDiscoverIDE_AmbiguousNoGuess(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writeLock(t, IDELock{PID: os.Getpid(), Port: 10, IDEName: "a", WorkspaceFolders: []string{"/a"}})
	writeLock(t, IDELock{PID: os.Getpid(), Port: 11, IDEName: "b", WorkspaceFolders: []string{"/b"}})
	if _, ok := DiscoverIDE("/tmp/unrelated"); ok {
		t.Error("two non-matching servers should be ambiguous → no guess")
	}
}

func TestIDELock_NoAuthHeaders(t *testing.T) {
	l := &IDELock{Port: 1}
	if h := l.AuthHeaders(); h != nil {
		t.Errorf("no token should yield nil headers, got %v", h)
	}
}

func portOf(l *IDELock) int {
	if l == nil {
		return -1
	}
	return l.Port
}

// TestDiscoverIDE_CrossLang validates the TS↔Go lockfile contract: a
// lockfile written by the VS Code extension's lockfile.ts must be
// discoverable here. Gated behind METIS_IDE_XLANG=1; the harness sets
// METIS_HOME to the dir the node writer used, METIS_IDE_XLANG_PORT to the
// expected port, and METIS_IDE_XLANG_WS to a workspace dir.
func TestDiscoverIDE_CrossLang(t *testing.T) {
	if os.Getenv("METIS_IDE_XLANG") != "1" {
		t.Skip("set METIS_IDE_XLANG=1 (and run via the cross-lang harness) to exercise the TS lockfile contract")
	}
	ws := os.Getenv("METIS_IDE_XLANG_WS")
	got, ok := DiscoverIDE(ws)
	if !ok {
		t.Fatal("DiscoverIDE did not find the TS-written lockfile")
	}
	if got.AuthToken == "" {
		t.Error("expected an authToken from the TS lockfile")
	}
	if got.Endpoint() == "" || got.Transport != "http" {
		t.Errorf("unexpected endpoint/transport: %q / %q", got.Endpoint(), got.Transport)
	}
	t.Logf("discovered %s at %s (auth len %d)", got.IDEName, got.Endpoint(), len(got.AuthToken))
}
