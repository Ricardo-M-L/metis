package slash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

// withTempMemdir overrides HOME so memdir.DefaultRoot resolves to a
// throwaway directory. Callers get back the resolved root.
func withTempMemdir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	root, err := memdir.DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if err := memdir.EnsureRoot(root); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	return root
}

func writeMemo(t *testing.T, root, name string, fm *memdir.Frontmatter, body string) {
	t.Helper()
	raw, err := memdir.RenderFile(fm, body)
	if err != nil {
		t.Fatalf("RenderFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, name), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestHandleMemoryCommand_EmptyShowsUsage(t *testing.T) {
	root := withTempMemdir(t)
	out := handleMemoryCommand("")
	if !strings.Contains(out, "empty") {
		t.Errorf("expected empty hint when no files; got %q", out)
	}
	_ = root
}

func TestHandleMemoryCommand_List(t *testing.T) {
	root := withTempMemdir(t)
	writeMemo(t, root, "user_role.md", &memdir.Frontmatter{
		Name: "User Role", Description: "Backend lead", Type: memdir.TypeUser,
	}, "body")
	out := handleMemoryCommand("list")
	if !strings.Contains(out, "user_role.md") || !strings.Contains(out, "Backend lead") {
		t.Errorf("manifest missing memo: %q", out)
	}
}

func TestHandleMemoryCommand_ShowAndRm(t *testing.T) {
	root := withTempMemdir(t)
	writeMemo(t, root, "user_x.md", &memdir.Frontmatter{
		Name: "X", Description: "x desc", Type: memdir.TypeUser,
	}, "secret body")

	out := handleMemoryCommand("show user_x.md")
	if !strings.Contains(out, "secret body") {
		t.Errorf("show should include body: %q", out)
	}

	// Show by bare basename (no .md) should also work.
	out2 := handleMemoryCommand("show user_x")
	if !strings.Contains(out2, "secret body") {
		t.Errorf("bare-basename show: %q", out2)
	}

	// rm
	out3 := handleMemoryCommand("rm user_x")
	if !strings.Contains(out3, "deleted") {
		t.Errorf("expected deletion confirm: %q", out3)
	}
	if _, err := os.Stat(filepath.Join(root, "user_x.md")); err == nil {
		t.Errorf("file still exists after rm")
	}
}

func TestHandleMemoryCommand_RmRegeneratesIndex(t *testing.T) {
	root := withTempMemdir(t)
	for _, n := range []string{"a", "b"} {
		writeMemo(t, root, n+".md", &memdir.Frontmatter{
			Name: n, Description: n, Type: memdir.TypeUser,
		}, "body")
	}
	// Seed index referencing both.
	files, _ := memdir.ScanMemoryFiles(context.Background(), root)
	_ = memdir.WriteIndex(root, files)

	handleMemoryCommand("rm a")
	idx, err := os.ReadFile(memdir.IndexPath(root))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if strings.Contains(string(idx), "a.md") {
		t.Errorf("index should not reference removed file: %q", idx)
	}
	if !strings.Contains(string(idx), "b.md") {
		t.Errorf("index should still reference surviving file: %q", idx)
	}
}

func TestHandleMemoryCommand_PathTraversalNormalizesToBasename(t *testing.T) {
	root := withTempMemdir(t)
	// `..` segments are stripped by filepath.Base before resolution,
	// so `../../etc/passwd.md` becomes `passwd.md` under root. The
	// system never reaches outside the memdir even when fed a
	// crafted input. We verify by checking that the operation does
	// NOT touch /etc and that the resulting message references the
	// memdir root.
	before, _ := os.Stat("/etc/passwd")
	out := handleMemoryCommand("rm ../../../../etc/passwd.md")
	after, _ := os.Stat("/etc/passwd")
	if before != nil && after == nil {
		t.Fatalf("path traversal removed /etc/passwd!")
	}
	if !strings.Contains(out, root) && !strings.Contains(out, "no such file") {
		t.Errorf("expected memdir-scoped error, got %q", out)
	}
}

func TestHandleMemoryCommand_Path(t *testing.T) {
	root := withTempMemdir(t)
	out := handleMemoryCommand("path")
	if out != root {
		t.Errorf("path = %q, want %q", out, root)
	}
}

func TestHandleMemoryCommand_UnknownSubcommand(t *testing.T) {
	withTempMemdir(t)
	out := handleMemoryCommand("frobnicate")
	if !strings.Contains(out, "unknown subcommand") {
		t.Errorf("got %q", out)
	}
}

func TestHandleMemoryCommand_ShowMissing(t *testing.T) {
	withTempMemdir(t)
	out := handleMemoryCommand("show nonexistent")
	if !strings.Contains(out, "memory show:") {
		t.Errorf("expected error message: %q", out)
	}
}
