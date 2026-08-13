package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffContextHonorsOutputLimit(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "large.txt")
	if err := os.WriteFile(path, []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewManager("bounded-diff", cwd, t.TempDir())
	before, err := manager.Snap("Edit", "before", "before large change")
	if err != nil || before == "" {
		t.Fatalf("Snap = %q, %v", before, err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("changed line\n", 20_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, err := manager.DiffContext(context.Background(), before, "", 2048)
	if !errors.Is(err, ErrDiffOutputLimit) {
		t.Fatalf("DiffContext error = %v, want ErrDiffOutputLimit", err)
	}
	if len(patch) > 2048 {
		t.Fatalf("bounded patch bytes = %d, want <= 2048", len(patch))
	}
}

func TestDiffContextHonorsCancellation(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "main.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewManager("cancel-diff", cwd, t.TempDir())
	before, err := manager.Snap("Edit", "before", "before cancellation")
	if err != nil || before == "" {
		t.Fatalf("Snap = %q, %v", before, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.DiffContext(ctx, before, "", 2048); !errors.Is(err, context.Canceled) {
		t.Fatalf("DiffContext canceled error = %v", err)
	}
}

func TestDiffReadsSnapshotsAndWorkingTreeWithoutCreatingCommits(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(cwd, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("main.go", "package old\n")
	manager := NewManager("diff-test", cwd, shadow)
	first, err := manager.Snap("Edit", "one", "before turn 1")
	if err != nil || first == "" {
		t.Fatalf("first Snap = %q, %v", first, err)
	}
	write("main.go", "package middle\n")
	second, err := manager.Snap("Edit", "two", "before turn 2")
	if err != nil || second == "" {
		t.Fatalf("second Snap = %q, %v", second, err)
	}

	between, err := manager.Diff(first, second)
	if err != nil || !strings.Contains(between, "-package old") || !strings.Contains(between, "+package middle") {
		t.Fatalf("snapshot diff err=%v:\n%s", err, between)
	}
	write("main.go", "package newest\n")
	write("new file.txt", "hello\n")
	before, err := manager.gitOutput("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Diff(second, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-package middle", "+package newest", "new file.txt", "+hello"} {
		if !strings.Contains(live, want) {
			t.Errorf("working-tree diff missing %q:\n%s", want, live)
		}
	}
	after, err := manager.gitOutput("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("read-only diff moved shadow HEAD: %s -> %s", before, after)
	}
}

func TestDiffDoesNotInitializeMissingShadowRepository(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	manager := NewManager("never-snapped", cwd, shadow)
	if _, err := manager.Diff(strings.Repeat("a", 40), ""); !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("Diff error = %v, want ErrSnapshotMissing", err)
	}
	if _, err := os.Stat(filepath.Join(shadow, "never-snapped", ".git")); !os.IsNotExist(err) {
		t.Fatalf("Diff initialized shadow repository: %v", err)
	}
}

func TestSnapAndDiffRecordDeletedFiles(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "obsolete.txt")
	if err := os.WriteFile(path, []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewManager("deleted-file", cwd, t.TempDir())
	before, err := manager.Snap("Edit", "before", "before deletion")
	if err != nil || before == "" {
		t.Fatalf("initial Snap = %q, %v", before, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	after, err := manager.Snap("Edit", "after", "after deletion")
	if err != nil || after == "" {
		t.Fatalf("deletion Snap = %q, %v", after, err)
	}
	patch, err := manager.Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"obsolete.txt", "deleted file mode", "-remove me"} {
		if !strings.Contains(patch, want) {
			t.Errorf("deletion diff missing %q:\n%s", want, patch)
		}
	}
}
