package skills

// realpath_dedup_test.go — pin the symlink-aware dedup added 2026-05-09.
// Without this, the same physical SKILL.md reachable via different
// paths (flat symlink + tree symlink, or two flat aliases) would emit
// twice and trigger spurious "X overrides X" log noise downstream.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirLayer_DedupBySymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics on Windows differ; skip")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	if err := os.WriteFile(target, []byte("---\nname: realone\ndescription: only-one\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two symlinks pointing at the same target — without dedup
	// they'd both be Load()'d and the loader would log "alias-of-real
	// overrides realone" (or the reverse) which is misleading.
	if err := os.Symlink(target, filepath.Join(dir, "alias1.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "alias2.md")); err != nil {
		t.Fatal(err)
	}

	layer := dirLayer("test", 1, dir, "")
	got, err := layer.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		names := make([]string, 0, len(got))
		for _, s := range got {
			names = append(names, s.Name)
		}
		t.Errorf("expected 1 skill (deduped); got %d: %v", len(got), names)
	}
	// The single survivor should keep "realone" name (manifest field
	// wins over filename-derived defaults). Whichever entry got
	// scanned first wins; we don't assert a particular order.
	if len(got) >= 1 && got[0].Name != "realone" {
		t.Errorf("survivor name = %q; want realone", got[0].Name)
	}
}

func TestDirLayer_DedupAcrossFlatAndTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics on Windows differ; skip")
	}
	dir := t.TempDir()
	// Tree layout: dir/cat/foo/SKILL.md
	treePath := filepath.Join(dir, "cat", "foo")
	if err := os.MkdirAll(treePath, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(treePath, "SKILL.md")
	if err := os.WriteFile(manifest, []byte("---\nname: foo\ndescription: tree-form\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Flat layout: dir/foo-alias.md → SAME target
	if err := os.Symlink(manifest, filepath.Join(dir, "foo-alias.md")); err != nil {
		t.Fatal(err)
	}

	layer := dirLayer("test", 1, dir, "")
	got, err := layer.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("flat+tree dedup: expected 1 skill; got %d", len(got))
	}
}

func TestCanonicalPath_HandlesBrokenSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics on Windows differ; skip")
	}
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.md")
	if err := os.Symlink("/nonexistent/target", broken); err != nil {
		t.Fatal(err)
	}
	got := canonicalPath(broken)
	// Broken symlink: EvalSymlinks errors, we fall back to the
	// abs original. Result must be non-empty and stable across calls.
	if got == "" {
		t.Errorf("canonicalPath returned empty for broken symlink")
	}
	if got2 := canonicalPath(broken); got2 != got {
		t.Errorf("canonicalPath not stable: %q vs %q", got, got2)
	}
}

func TestDirLayer_NoSymlinks_StillWorks(t *testing.T) {
	// Regression guard: the dedup code path must not break the
	// no-symlink common case.
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		path := filepath.Join(dir, name)
		body := "---\nname: " + name[:1] + "\ndescription: t\n---\nbody\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	layer := dirLayer("test", 1, dir, "")
	got, err := layer.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 distinct skills; got %d", len(got))
	}
}
