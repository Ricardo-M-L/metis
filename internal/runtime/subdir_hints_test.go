package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtractAtMentionPaths(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"hello world", nil},
		{"check @cmd/main.go please", []string{"cmd/main.go"}},
		{"@a.go and @b.go", []string{"a.go", "b.go"}},
		{"look at @cmd/main.go.", []string{"cmd/main.go"}},                          // trailing dot trimmed
		{"see @services/foo,@services/bar", []string{"services/foo,@services/bar"}}, // no space → one token
		{"email like a@b.com is NOT a mention", nil},
		{"\n@root.md", []string{"root.md"}},
		{"@.metis/CLAUDE.md", []string{".metis/CLAUDE.md"}},
		{"`@quoted` won't be cleaned but @real does", []string{"real"}},
	}
	for _, c := range cases {
		got := extractAtMentionPaths(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("extract(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestCollectSubdirHints_DownWalkPicksUpNestedFiles(t *testing.T) {
	root := t.TempDir()
	// Nested layout:
	//   <root>/CLAUDE.md            (repo-level, in cwd — should NOT show in hints)
	//   <root>/services/CLAUDE.md   (mid-level)
	//   <root>/services/foo/AGENTS.md  (leaf-level)
	//   <root>/services/foo/bar.go     (the @-mentioned file)
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "# repo level — should NOT appear")
	mustMkdir(t, filepath.Join(root, "services"))
	mustWrite(t, filepath.Join(root, "services", "CLAUDE.md"), "# services-layer convention\nuse pgx, not pq")
	mustMkdir(t, filepath.Join(root, "services", "foo"))
	mustWrite(t, filepath.Join(root, "services", "foo", "AGENTS.md"), "# foo-service AGENTS\ntimeout=30s")
	mustWrite(t, filepath.Join(root, "services", "foo", "bar.go"), "package foo")

	got := CollectSubdirHints("please read @services/foo/bar.go", root, nil)
	if got == "" {
		t.Fatal("CollectSubdirHints returned empty; expected hints")
	}
	if !strings.Contains(got, "services-layer convention") {
		t.Errorf("missing services-level hint; got:\n%s", got)
	}
	if !strings.Contains(got, "foo-service AGENTS") {
		t.Errorf("missing foo-level hint; got:\n%s", got)
	}
	if strings.Contains(got, "repo level — should NOT appear") {
		t.Errorf("cwd-level hint leaked into subdir block (loadProjectContext should own it):\n%s", got)
	}
	if !strings.HasPrefix(got, "<subdirectory_hints>") {
		t.Errorf("hints block missing outer tag; got prefix %q", got[:min(40, len(got))])
	}
	if !strings.HasSuffix(got, "</subdirectory_hints>") {
		t.Errorf("hints block missing closing tag")
	}
}

func TestCollectSubdirHints_NoMentionNoHints(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "x"))
	mustWrite(t, filepath.Join(root, "x", "CLAUDE.md"), "# x")

	got := CollectSubdirHints("how do I configure timeouts?", root, nil)
	if got != "" {
		t.Errorf("expected empty (no @-mentions); got:\n%s", got)
	}
}

func TestCollectSubdirHints_MentionEscapingCwdRejected(t *testing.T) {
	root := t.TempDir()
	above := filepath.Dir(root)
	// Plant a file in the parent of cwd; @-mention should NOT descend
	// into it (escapes cwd → rejected).
	if err := os.WriteFile(filepath.Join(above, "CLAUDE.md"), []byte("# parent — must not leak"), 0o600); err != nil {
		t.Skip("can't write to tempdir parent on this platform")
	}
	defer os.Remove(filepath.Join(above, "CLAUDE.md"))

	got := CollectSubdirHints("see @../CLAUDE.md", root, nil)
	if strings.Contains(got, "parent — must not leak") {
		t.Errorf("parent CLAUDE.md leaked into subdir hints (escaping cwd should be rejected):\n%s", got)
	}
}

func TestCollectSubdirHints_DedupViaAlreadyLoaded(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "a"))
	hintPath := filepath.Join(root, "a", "CLAUDE.md")
	mustWrite(t, hintPath, "# a-level\nuse pgx")
	mustWrite(t, filepath.Join(root, "a", "file.go"), "package a")

	// Caller pre-populates seen with a/CLAUDE.md → no emission.
	canon := canonicalPath(hintPath)
	already := map[string]bool{canon: true}
	got := CollectSubdirHints("see @a/file.go", root, already)
	if got != "" {
		t.Errorf("expected empty when alreadyLoaded carries the hint; got:\n%s", got)
	}
}

func TestCollectSubdirHints_MultipleMentionsDedup(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "a"))
	mustWrite(t, filepath.Join(root, "a", "CLAUDE.md"), "# a-conv")
	mustWrite(t, filepath.Join(root, "a", "x.go"), "package a")
	mustWrite(t, filepath.Join(root, "a", "y.go"), "package a")

	got := CollectSubdirHints("look at @a/x.go and @a/y.go", root, nil)
	// CLAUDE.md should appear once, not twice — both mentions hit the
	// same directory.
	if c := strings.Count(got, "a-conv"); c != 1 {
		t.Errorf("expected dedup'd hint to appear once; saw %d times:\n%s", c, got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
