package checkpoint

// checkpoint_test.go — pin Snap → List → Restore round-trip plus
// the obvious failure modes (skip dirs, big files, init-error
// disables manager).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// freshManager builds a Manager pointing at a fresh tempdir cwd
// and a separate tempdir for the shadow root.
func freshManager(t *testing.T) (*Manager, string) {
	t.Helper()
	cwd := t.TempDir()
	shadow := t.TempDir()
	return NewManager("session-test", cwd, shadow), cwd
}

func TestSnap_NewSessionRecordsFirstCheckpoint(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := m.Snap("Edit", HashArgs(map[string]any{"file": "a.txt"}), "wrote a.txt")
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}
	if len(hash) < 7 {
		t.Errorf("expected commit hash; got %q", hash)
	}
}

func TestSnap_NoChangesReturnsEmptyHash(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	_ = os.WriteFile(filepath.Join(cwd, "x.txt"), []byte("x"), 0o600)
	if _, err := m.Snap("Edit", "k1", "first"); err != nil {
		t.Fatalf("first Snap: %v", err)
	}
	hash2, err := m.Snap("Edit", "k1", "second")
	if err != nil {
		t.Fatalf("second Snap: %v", err)
	}
	if hash2 != "" {
		t.Errorf("Snap with no changes should return empty hash; got %q", hash2)
	}
}

func TestList_ReturnsRecentCheckpoints(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	for i := 0; i < 3; i++ {
		_ = os.WriteFile(filepath.Join(cwd, "f.txt"), []byte(strings.Repeat("a", i+1)), 0o600)
		if _, err := m.Snap("Edit", "args"+string(rune('0'+i)), "msg"+string(rune('0'+i))); err != nil {
			t.Fatalf("snap %d: %v", i, err)
		}
	}
	cps, err := m.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cps) != 3 {
		t.Errorf("len(cps)=%d; want 3", len(cps))
	}
	// Newest first per `git log`.
	if cps[0].ArgsKey != "args2" {
		t.Errorf("expected newest first (args2); got %q", cps[0].ArgsKey)
	}
}

func TestRestore_RoundTripsContent(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	path := filepath.Join(cwd, "main.go")

	_ = os.WriteFile(path, []byte("v1"), 0o600)
	if _, err := m.Snap("Write", "k1", "v1"); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(path, []byte("v2"), 0o600)
	hash2, err := m.Snap("Write", "k2", "v2")
	if err != nil {
		t.Fatal(err)
	}

	// Now mutate to v3 (uncommitted) and restore back to v2's snapshot.
	_ = os.WriteFile(path, []byte("v3"), 0o600)
	if err := m.Restore(hash2); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "v2" {
		t.Errorf("after restore body=%q; want v2", body)
	}
}

func TestSnap_SkipsBigFiles(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	// 2 MiB > maxFileBytes — should NOT make it into the shadow.
	big := make([]byte, 2<<20)
	if err := os.WriteFile(filepath.Join(cwd, "big.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "small.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := m.Snap("Write", "k", "test")
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}
	if hash == "" {
		t.Skip("no commit recorded — small.txt skipped too?")
	}
	// Verify big.bin NOT in the shadow tree.
	if _, err := os.Stat(filepath.Join(m.shadowDir, "big.bin")); err == nil {
		t.Errorf("big.bin should be skipped (>1MiB)")
	}
	if _, err := os.Stat(filepath.Join(m.shadowDir, "small.txt")); err != nil {
		t.Errorf("small.txt should be in shadow; %v", err)
	}
}

func TestSnap_SkipsKnownDirs(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	// Create a node_modules subdir with content — must be skipped.
	nm := filepath.Join(cwd, "node_modules", "foo")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "x.js"), []byte("dep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snap("Write", "k", "test"); err != nil {
		t.Fatalf("Snap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.shadowDir, "node_modules", "foo", "x.js")); err == nil {
		t.Errorf("node_modules should be skipped")
	}
}

// TestSnap_SkipsMetisDir — the self-recursion fix (2026-06-14). A
// `.metis` subtree under cwd (which contains the shadow dir itself when
// metis runs from home) must be pruned, or copyTree nests the shadow
// into itself until paths overflow the 255-byte filename limit.
func TestSnap_SkipsMetisDir(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	mdir := filepath.Join(cwd, ".metis", "checkpoints", "abc")
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mdir, "shadow.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "real.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snap("Write", "k", "test"); err != nil {
		t.Fatalf("Snap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.shadowDir, ".metis")); err == nil {
		t.Error(".metis must be pruned (self-recursion guard)")
	}
	if _, err := os.Stat(filepath.Join(m.shadowDir, "real.txt")); err != nil {
		t.Error("non-.metis content should still be snapshotted")
	}
}

// TestSnap_NeverRecursesIntoShadowUnderCwd — the defensive backstop:
// even with a custom shadowRoot living UNDER cwd (not named .metis),
// copyTree must not copy the shadow dir into itself. Two snaps that
// would otherwise nest deeper each time.
func TestSnap_NeverRecursesIntoShadowUnderCwd(t *testing.T) {
	skipIfNoGit(t)
	cwd := t.TempDir()
	shadowRoot := filepath.Join(cwd, "myshadow") // under cwd, non-.metis name
	m := NewManager("session-test", cwd, shadowRoot)
	if err := os.WriteFile(filepath.Join(cwd, "real.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snap("Write", "k1", "test"); err != nil {
		t.Fatalf("Snap1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "real.txt"), []byte("ok2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snap("Write", "k2", "test"); err != nil {
		t.Fatalf("Snap2: %v", err)
	}
	// The shadow must not contain a copy of itself.
	if _, err := os.Stat(filepath.Join(m.shadowDir, "myshadow")); err == nil {
		t.Error("shadow recursed into itself — self-copy was not pruned")
	}
}

// TestNewManager_DisablesOnUnsafeRoot — running from ~ or / must
// disable checkpointing so we never snapshot the entire home tree
// (the root cause of the slow `find ~` + huge shadow, 2026-06-14).
func TestNewManager_DisablesOnUnsafeRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		cwd          string
		wantDisabled bool
	}{
		{home, true},                              // bare home
		{home + "/", true},                        // home with trailing slash (Clean normalizes)
		{"/", true},                               // filesystem root
		{"", true},                                // empty (defensive)
		{filepath.Join(home, "Documents", "p"), false}, // a real project subdir
		{t.TempDir(), false},                      // an unrelated dir
	}
	for _, c := range cases {
		m := NewManager("s", c.cwd, t.TempDir())
		if got := m.Disabled(); got != c.wantDisabled {
			t.Errorf("NewManager(cwd=%q).Disabled() = %v, want %v", c.cwd, got, c.wantDisabled)
		}
	}
}

// A disabled manager must make Snap a fast no-op (never touch disk).
func TestSnap_NoOpWhenDisabledOnHome(t *testing.T) {
	skipIfNoGit(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	shadow := t.TempDir()
	m := NewManager("s", home, shadow)
	hash, err := m.Snap("Write", "k", "test")
	if err == nil {
		t.Error("Snap on a home-cwd manager should return the disabled error")
	}
	if hash != "" {
		t.Errorf("disabled Snap returned a hash %q", hash)
	}
	// Shadow dir must stay empty — nothing snapshotted.
	entries, _ := os.ReadDir(filepath.Join(shadow, "s"))
	if len(entries) != 0 {
		t.Errorf("disabled manager wrote %d entries to shadow", len(entries))
	}
}

func TestHashArgs_Deterministic(t *testing.T) {
	a := HashArgs(map[string]any{"file_path": "x", "content": "y"})
	b := HashArgs(map[string]any{"content": "y", "file_path": "x"})
	if a != b {
		t.Errorf("HashArgs not stable across map iter order: %s vs %s", a, b)
	}
}

func TestHashArgs_NoArgs(t *testing.T) {
	if got := HashArgs(nil); got != "noargs" {
		t.Errorf("nil → got %q; want noargs", got)
	}
}

func TestParseCheckpoint_RoundTrip(t *testing.T) {
	subject := "2026-05-09T02:00:00Z|Edit|abcd1234|wrote main.go"
	cp := parseCheckpoint("hashabc", subject)
	if cp.Tool != "Edit" {
		t.Errorf("Tool=%q; want Edit", cp.Tool)
	}
	if cp.ArgsKey != "abcd1234" {
		t.Errorf("ArgsKey=%q; want abcd1234", cp.ArgsKey)
	}
	if cp.Message != "wrote main.go" {
		t.Errorf("Message=%q", cp.Message)
	}
	if cp.Time.IsZero() {
		t.Errorf("Time should parse; got zero")
	}
}

// TestIsUnsafeCheckpointRoot_SymlinkedHome — a cwd that reaches home
// through a symlink (or macOS firmlink) must still be detected, so the
// whole-home snapshot can't slip past the bare string compare
// (2026-06-14 review finding).
func TestIsUnsafeCheckpointRoot_SymlinkedHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	// Build a symlink that points at home, then check a cwd expressed
	// through the symlink is flagged unsafe.
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("cannot symlink: %v", err)
	}
	if !isUnsafeCheckpointRoot(link) {
		t.Error("a symlink resolving to home should be flagged unsafe")
	}
	// A symlink to a NON-home dir must stay safe.
	other := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(other, 0o755)
	link2 := filepath.Join(t.TempDir(), "projlink")
	if err := os.Symlink(other, link2); err == nil {
		if isUnsafeCheckpointRoot(link2) {
			t.Error("a symlink to a normal project dir must not be flagged unsafe")
		}
	}
}
