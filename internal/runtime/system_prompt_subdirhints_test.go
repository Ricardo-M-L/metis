package runtime

// system_prompt_subdirhints_test.go — pin the multi-file project-
// context accumulator added 2026-05-09 (#9 SUMMARY).
//
// The previous loadProjectContext stopped at the first matching file
// it found while walking cwd → git root. The hermes-style change
// gathers EVERY file on that path so a monorepo's nested AGENTS.md
// + repo-wide CLAUDE.md both inform the model.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCwd is a small test fixture: chdir to dir, return a cleanup.
func withCwd(t *testing.T, dir string) {
	t.Helper()
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestLoadProjectContext_MultiFileWalk(t *testing.T) {
	root := t.TempDir()
	// Mock a git root.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Repo-level CLAUDE.md
	repoMD := filepath.Join(root, "CLAUDE.md")
	if err := os.WriteFile(repoMD, []byte("# repo-level conventions\nuse tabs"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Service-level AGENTS.md (one level down)
	svcDir := filepath.Join(root, "services", "foo")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	svcMD := filepath.Join(svcDir, "AGENTS.md")
	if err := os.WriteFile(svcMD, []byte("# service foo\ntimeout=30s"), 0o600); err != nil {
		t.Fatal(err)
	}

	withCwd(t, svcDir)

	got := loadProjectContext()
	if got == "" {
		t.Fatalf("loadProjectContext returned empty; expected both files")
	}

	// Both file bodies should be present.
	if !strings.Contains(got, "service foo") {
		t.Errorf("missing service-level body; got %q", got)
	}
	if !strings.Contains(got, "repo-level conventions") {
		t.Errorf("missing repo-level body; got %q", got)
	}

	// More-specific (services/foo) MUST come before less-specific (root).
	idxSvc := strings.Index(got, "service foo")
	idxRepo := strings.Index(got, "repo-level conventions")
	if idxSvc > idxRepo {
		t.Errorf("ordering wrong: service block at %d should precede repo block at %d", idxSvc, idxRepo)
	}

	// Each file gets its own <project_context> wrapper.
	if c := strings.Count(got, "<project_context "); c != 2 {
		t.Errorf("expected 2 <project_context> wrappers; got %d in:\n%s", c, got)
	}
}

func TestLoadProjectContext_DedupSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(target, []byte("# unique-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Place a symlinked CLAUDE.md → AGENTS.md in same dir.
	link := filepath.Join(root, "CLAUDE.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	withCwd(t, root)

	got := loadProjectContext()
	// Dedup should keep the file body once. We see exactly one
	// occurrence of the unique marker.
	if c := strings.Count(got, "unique-content"); c != 1 {
		t.Errorf("dedup failed: 'unique-content' appears %d times in:\n%s", c, got)
	}
}

func TestLoadProjectContext_NoneFound(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root)
	got := loadProjectContext()
	if got != "" {
		t.Errorf("no project files → expected empty; got %q", got)
	}
}

func TestCanonicalPath_StableForBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken")
	if err := os.Symlink("/nonexistent", broken); err != nil {
		t.Skip("symlinks not supported")
	}
	a := canonicalPath(broken)
	b := canonicalPath(broken)
	if a != b {
		t.Errorf("canonicalPath not stable: %q vs %q", a, b)
	}
}
