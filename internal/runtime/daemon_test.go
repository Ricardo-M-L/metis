package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunDaemon_ProcessesInboxFile — drop a task into inbox/, run the
// daemon for ~1.5 polls, verify result lands in outbox/ and inbox file
// is deleted.
func TestRunDaemon_ProcessesInboxFile(t *testing.T) {
	dir := t.TempDir()
	cfg := DaemonConfig{
		InboxDir:     filepath.Join(dir, "inbox"),
		OutboxDir:    filepath.Join(dir, "outbox"),
		PidFile:      filepath.Join(dir, "daemon.pid"),
		StatusFile:   filepath.Join(dir, "daemon.status.json"),
		PollInterval: 100 * time.Millisecond,
		IdleTimeout:  1 * time.Hour, // never fires in this test
	}
	_ = os.MkdirAll(cfg.InboxDir, 0o755)
	_ = os.WriteFile(filepath.Join(cfg.InboxDir, "task1.txt"), []byte("hello world"), 0o644)

	var seen atomic.Value
	handler := func(_ context.Context, prompt string) (string, error) {
		seen.Store(prompt)
		return "PROCESSED: " + prompt, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = RunDaemon(ctx, cfg, handler, nil)

	if got := seen.Load(); got != "hello world" {
		t.Errorf("handler should see the task body; got %v", got)
	}
	out, err := os.ReadFile(filepath.Join(cfg.OutboxDir, "task1.txt"))
	if err != nil {
		t.Fatalf("outbox file missing: %v", err)
	}
	if string(out) != "PROCESSED: hello world" {
		t.Errorf("outbox content mismatch: %q", string(out))
	}
	// Inbox file must be deleted.
	if _, err := os.Stat(filepath.Join(cfg.InboxDir, "task1.txt")); !os.IsNotExist(err) {
		t.Errorf("inbox file should be deleted; stat err=%v", err)
	}
}

// TestRunDaemon_FIFOOrderByMtime — multiple tasks process oldest-first
// so the daemon survives restarts without reordering work.
func TestRunDaemon_FIFOOrderByMtime(t *testing.T) {
	dir := t.TempDir()
	cfg := DaemonConfig{
		InboxDir:     filepath.Join(dir, "inbox"),
		OutboxDir:    filepath.Join(dir, "outbox"),
		PidFile:      filepath.Join(dir, "pid"),
		StatusFile:   filepath.Join(dir, "status.json"),
		PollInterval: 100 * time.Millisecond,
		IdleTimeout:  1 * time.Hour,
	}
	_ = os.MkdirAll(cfg.InboxDir, 0o755)
	now := time.Now()
	files := []string{"second.txt", "first.txt", "third.txt"}
	mtimes := []time.Time{now.Add(-1 * time.Second), now.Add(-2 * time.Second), now.Add(-500 * time.Millisecond)}
	for i, f := range files {
		path := filepath.Join(cfg.InboxDir, f)
		_ = os.WriteFile(path, []byte("body-"+f), 0o644)
		_ = os.Chtimes(path, mtimes[i], mtimes[i])
	}

	var order []string
	handler := func(_ context.Context, prompt string) (string, error) {
		order = append(order, prompt)
		return "ok", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_ = RunDaemon(ctx, cfg, handler, nil)

	want := []string{"body-first.txt", "body-second.txt", "body-third.txt"}
	if len(order) != 3 {
		t.Fatalf("expected 3 tasks processed in FIFO order; got %d (%v)", len(order), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("FIFO violated at %d: got %q want %q (full order %v)", i, order[i], want[i], order)
		}
	}
}

// TestRunDaemon_IdleTriggersDistill — empty inbox + IdleTimeout elapsed
// → distill handler fires once.
func TestRunDaemon_IdleTriggersDistill(t *testing.T) {
	dir := t.TempDir()
	cfg := DaemonConfig{
		InboxDir:     filepath.Join(dir, "inbox"),
		OutboxDir:    filepath.Join(dir, "outbox"),
		PidFile:      filepath.Join(dir, "pid"),
		StatusFile:   filepath.Join(dir, "status.json"),
		PollInterval: 50 * time.Millisecond,
		IdleTimeout:  100 * time.Millisecond, // fast for tests
	}
	_ = os.MkdirAll(cfg.InboxDir, 0o755)
	var distillRuns int32
	distill := func(_ context.Context) error {
		atomic.AddInt32(&distillRuns, 1)
		return nil
	}
	handler := func(_ context.Context, _ string) (string, error) { return "", nil }

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = RunDaemon(ctx, cfg, handler, distill)

	if atomic.LoadInt32(&distillRuns) == 0 {
		t.Errorf("distill should have fired at least once during idle")
	}
}
