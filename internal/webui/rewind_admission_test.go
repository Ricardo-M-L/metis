package webui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRewindTreeLeaseExcludesRunningChildrenAndFutureWriters(t *testing.T) {
	c := NewTurnCoordinator(8)
	home := t.TempDir()
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	child, ok := c.TryAcquire(project)
	if !ok {
		t.Fatal("project lease unavailable")
	}
	if lease, ok := c.TryAcquireRewind(home); ok {
		lease.Release()
		t.Fatal("home restore raced its running child project")
	}
	// Ordinary turns keep existing per-workspace admission, even with a home
	// session: this stricter boundary belongs only to explicit file restores.
	ordinaryHome, ok := c.TryAcquire(home)
	if !ok {
		t.Fatal("ordinary workspace admission was broadened unexpectedly")
	}
	ordinaryHome.Release()
	child.Release()
	restore, ok := c.TryAcquireRewind(home)
	if !ok {
		t.Fatal("idle home restore lease unavailable")
	}
	if lease, ok := c.TryAcquire(project); ok {
		lease.Release()
		t.Fatal("new child writer raced an active home restore")
	}
	if lease, ok := c.TryAcquire(filepath.Dir(home)); ok {
		lease.Release()
		t.Fatal("new ancestor writer raced an active home restore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if lease, err := c.Acquire(ctx, project); err == nil {
		lease.Release()
		t.Fatal("queued child bypassed the restore lease")
	}
	other, ok := c.TryAcquire(home + "-unrelated")
	if !ok {
		t.Fatal("unrelated workspace was blocked")
	}
	other.Release()
	restore.Release()
	child, ok = c.TryAcquire(project)
	if !ok {
		t.Fatal("restore did not release child workspace admission")
	}
	child.Release()
}

func TestRewindTreeLeaseDoesNotConfuseSiblingPathPrefixes(t *testing.T) {
	c := NewTurnCoordinator(4)
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "a-sibling"), 0o700); err != nil {
		t.Fatal(err)
	}
	restore, ok := c.TryAcquireRewind(filepath.Join(home, "a"))
	if !ok {
		t.Fatal("restore unavailable")
	}
	defer restore.Release()
	sibling, ok := c.TryAcquire(filepath.Join(home, "a-sibling"))
	if !ok {
		t.Fatal("prefix sibling treated as descendant")
	}
	defer sibling.Release()
	if other, ok := c.TryAcquireRewind(home); ok {
		other.Release()
		t.Fatal("ancestor restore ignored active descendant restore")
	}
}
