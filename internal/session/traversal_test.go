package session

import (
	"path/filepath"
	"strings"
	"testing"
)

// A crafted session id with path-traversal sequences must never resolve to
// a path outside the store's Dir.
func TestStorePath_NoTraversal(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	for _, id := range []string{
		"../../../../tmp/evil",
		"../config",
		"..",
		"a/b/c",
		"/etc/passwd",
	} {
		p := s.path(id)
		clean := filepath.Clean(p)
		if !strings.HasPrefix(clean, filepath.Clean(dir)+string(filepath.Separator)) {
			t.Errorf("path(%q) = %q escaped Dir %q", id, clean, dir)
		}
	}
	// A normal id is unaffected.
	if got := s.path("session-123"); got != filepath.Join(dir, "session-123.jsonl") {
		t.Errorf("normal id mangled: %q", got)
	}
}
