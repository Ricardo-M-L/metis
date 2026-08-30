package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowedDirs_AddRoundTrip(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)
	tmp := t.TempDir()

	if err := d.Add(tmp, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	want, err := normalizeDir(tmp)
	if err != nil {
		t.Fatalf("normalize temp dir: %v", err)
	}
	got := d.All()
	if len(got) != 1 || got[0] != want {
		t.Errorf("All() = %v, want [%q]", got, want)
	}

	// Re-add same path is idempotent.
	if err := d.Add(tmp, false); err != nil {
		t.Errorf("re-Add should be idempotent, got: %v", err)
	}
	if len(d.All()) != 1 {
		t.Errorf("re-Add caused dup: %v", d.All())
	}
}

func TestAllowedDirs_PersistAcrossNew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	tmp := t.TempDir()

	d1 := NewAllowedDirs(nil)
	if err := d1.Add(tmp, true); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d2 := NewAllowedDirs(nil)
	want, err := normalizeDir(tmp)
	if err != nil {
		t.Fatalf("normalize temp dir: %v", err)
	}
	got := d2.All()
	if len(got) != 1 || got[0] != want {
		t.Errorf("after reload All() = %v, want [%q]", got, want)
	}
}

func TestAllowedDirs_RejectsNonDir(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)

	tmpfile := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(tmpfile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Add(tmpfile, false); err == nil {
		t.Errorf("Add(file) should error, got nil")
	}
	if err := d.Add("/nonexistent/path/should/never/exist", false); err == nil {
		t.Errorf("Add(missing) should error, got nil")
	}
}

func TestAllowedDirs_SystemPromptAddendum(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)
	if got := d.SystemPromptAddendum(); got != "" {
		t.Errorf("empty list should give empty addendum, got %q", got)
	}
	tmp := t.TempDir()
	_ = d.Add(tmp, false)
	got := d.SystemPromptAddendum()
	if !strings.Contains(got, tmp) {
		t.Errorf("addendum should mention %q, got %q", tmp, got)
	}
	if !strings.Contains(got, "Additional accessible directories") {
		t.Errorf("addendum missing header: %q", got)
	}
}

func TestAllowedDirs_RemoveErrorsOnMissing(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	d := NewAllowedDirs(nil)
	if err := d.Remove("/tmp"); err == nil {
		t.Errorf("Remove of unknown dir should error")
	}
}

func TestAllowedDirs_ContainsLaunchCWDAndAdditionalRoots(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	root := t.TempDir()
	extra := t.TempDir()
	d := newAllowedDirs(root, nil)

	canonicalRoot, err := normalizeDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Scope(); len(got) != 1 || got[0] != canonicalRoot {
		t.Fatalf("initial Scope() = %v, want cwd %q", got, canonicalRoot)
	}
	if !d.Contains(filepath.Join(root, "new", "file.go")) {
		t.Fatal("new file below launch cwd should be in scope")
	}
	if !d.Contains(filepath.Join("relative", "file.go")) {
		t.Fatal("relative path should resolve below launch cwd")
	}
	if d.Contains(filepath.Dir(root)) {
		t.Fatal("cwd parent must not be in scope")
	}

	if err := d.Add(extra, false); err != nil {
		t.Fatal(err)
	}
	if !d.Contains(filepath.Join(extra, "new.txt")) {
		t.Fatal("file below --add-dir root should be in scope")
	}
	if err := d.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if d.Contains(filepath.Join(extra, "new.txt")) {
		t.Fatal("removed additional root must leave scope immediately")
	}
}

func TestAllowedDirs_PreparedRebindDoesNotPublishBeforeCommit(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	source := t.TempDir()
	target := t.TempDir()
	d := newAllowedDirs(source, nil)

	prepared, err := d.PrepareRebindCWD(target)
	if err != nil {
		t.Fatalf("PrepareRebindCWD: %v", err)
	}
	if !d.Contains(filepath.Join(source, "source.txt")) || d.Contains(filepath.Join(target, "target.txt")) {
		t.Fatalf("prepare changed live scope: %v", d.Scope())
	}

	prepared.Commit()
	if !d.Contains(filepath.Join(target, "target.txt")) || d.Contains(filepath.Join(source, "source.txt")) {
		t.Fatalf("commit did not atomically replace cwd scope: %v", d.Scope())
	}
}

func TestAllowedDirs_PreparedRebindExposesPinnedCanonicalPath(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}

	prepared, err := newAllowedDirs(parent, nil).PrepareRebindCWD(alias)
	if err != nil {
		t.Fatalf("PrepareRebindCWD: %v", err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.CanonicalPath(); got != want {
		t.Fatalf("CanonicalPath() = %q, want %q", got, want)
	}
}

func TestAllowedDirs_FailedPreparedRebindLeavesScopeUnchanged(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	source := t.TempDir()
	d := newAllowedDirs(source, nil)
	missing := filepath.Join(t.TempDir(), "deleted-workspace")

	if prepared, err := d.PrepareRebindCWD(missing); err == nil || prepared != nil {
		t.Fatalf("missing workspace prepare = (%v, %v), want nil,error", prepared, err)
	}
	if !d.Contains(filepath.Join(source, "still-authorized.txt")) {
		t.Fatalf("failed prepare changed source scope: %v", d.Scope())
	}
}

func TestAllowedDirs_ContainsRejectsDotDotAndPrefixSibling(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	sibling := filepath.Join(parent, "repo-secret")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newAllowedDirs(root, nil)

	if d.Contains(filepath.Join(root, "..", "repo-secret", "key")) {
		t.Fatal(".. traversal escaped cwd scope")
	}
	if d.Contains(filepath.Join(sibling, "key")) {
		t.Fatal("component-prefix sibling was treated as a child")
	}
}

func TestAllowedDirs_ContainsResolvesExistingAndDanglingSymlinks(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "inside-link")); err != nil {
		t.Fatal(err)
	}
	// The target file deliberately does not exist. EvalSymlinks alone would
	// fail and a lexical-prefix fallback would incorrectly allow this Write.
	danglingTarget := filepath.Join(outside, "not-created-yet.txt")
	if err := os.Symlink(danglingTarget, filepath.Join(root, "dangling-link")); err != nil {
		t.Fatal(err)
	}

	d := newAllowedDirs(root, nil)
	if d.Contains(filepath.Join(root, "outside-link", "secret")) {
		t.Fatal("existing symlink escaped cwd scope")
	}
	if d.Contains(filepath.Join(root, "dangling-link")) {
		t.Fatal("dangling symlink to an outside write target escaped cwd scope")
	}
	if !d.Contains(filepath.Join(root, "inside-link", "new.txt")) {
		t.Fatal("symlink resolving back inside cwd should remain in scope")
	}
}
