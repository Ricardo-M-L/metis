package memdir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMemo(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestScanMemoryFiles_NonExistentRoot(t *testing.T) {
	got, err := ScanMemoryFiles(context.Background(), filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("ScanMemoryFiles: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty for missing root, got %d", len(got))
	}
}

func TestScanMemoryFiles_SkipsIndexAndNonMD(t *testing.T) {
	dir := t.TempDir()
	writeMemo(t, dir, "MEMORY.md", "- [a](a.md)\n")
	writeMemo(t, dir, "a.md", "---\nname: a\ndescription: A\ntype: user\n---\nbody\n")
	writeMemo(t, dir, "notes.txt", "ignored")
	got, err := ScanMemoryFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ScanMemoryFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 file (a.md), got %d: %+v", len(got), got)
	}
	if got[0].Name != "a" {
		t.Fatalf("Name = %q, want a", got[0].Name)
	}
}

func TestScanMemoryFiles_OrdersByModTimeDesc(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"old.md", "new.md"} {
		writeMemo(t, dir, n, "---\nname: "+n+"\ndescription: x\ntype: user\n---\n")
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "old.md"), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	got, err := ScanMemoryFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ScanMemoryFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count: %d", len(got))
	}
	if got[0].Name != "new" {
		t.Fatalf("first should be new, got %s", got[0].Name)
	}
}

func TestScanMemoryFiles_ParseErrorIsCaptured(t *testing.T) {
	dir := t.TempDir()
	writeMemo(t, dir, "bad.md", "---\nname: [oops\n---\nbody\n")
	writeMemo(t, dir, "good.md", "---\nname: g\ndescription: G\ntype: user\n---\nbody\n")
	got, err := ScanMemoryFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ScanMemoryFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count: %d", len(got))
	}
	var hadErr bool
	for _, f := range got {
		if f.Name == "bad" && f.ParseError != nil {
			hadErr = true
		}
	}
	if !hadErr {
		t.Fatalf("expected bad.md to carry ParseError, got %+v", got)
	}
}

func TestFilterByType(t *testing.T) {
	files := []MemoryFile{
		{Name: "u", Frontmatter: Frontmatter{Type: TypeUser}},
		{Name: "f", Frontmatter: Frontmatter{Type: TypeFeedback}},
		{Name: "u2", Frontmatter: Frontmatter{Type: TypeUser}},
	}
	got := FilterByType(files, TypeUser)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestMemoryFile_TitleAndType(t *testing.T) {
	mf := MemoryFile{Name: "fallback", Frontmatter: Frontmatter{Name: "Real Title", Type: TypeProject}}
	if mf.Title() != "Real Title" {
		t.Fatalf("Title = %q", mf.Title())
	}
	if mf.Type() != TypeProject {
		t.Fatalf("Type = %q", mf.Type())
	}
	mf2 := MemoryFile{Name: "fallback"}
	if mf2.Title() != "fallback" {
		t.Fatalf("Title fallback = %q", mf2.Title())
	}
}
