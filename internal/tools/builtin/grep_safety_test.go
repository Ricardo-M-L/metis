//go:build !windows

package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func newGrep() Grep { return Grep{gate: permission.New(permission.ModeBypass)} }

// runGrep executes with a hard timeout so a regression that re-introduces the
// "Grep hangs forever" bug fails fast instead of stalling the whole suite.
func runGrep(t *testing.T, ctx context.Context, in map[string]any) string {
	t.Helper()
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := newGrep().Execute(ctx, in)
		out := ""
		if r != nil {
			out = r.Output
		}
		ch <- res{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("grep error: %v", r.err)
		}
		return r.out
	case <-time.After(10 * time.Second):
		t.Fatal("Grep did not return within 10s — likely hung on a special/huge file")
		return ""
	}
}

// A FIFO (named pipe) in the search tree must be skipped, not opened —
// os.Open on a FIFO with no writer blocks forever. This is the exact hang
// the user hit (a stray Grep from $HOME).
func TestGrep_SkipsFifoWithoutHanging(t *testing.T) {
	tmp := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(tmp, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("MATCH here\n"), 0o644)

	out := runGrep(t, context.Background(), map[string]any{"pattern": "MATCH", "root": tmp})
	if !strings.Contains(out, "MATCH here") {
		t.Errorf("should find the match in the regular file, got: %q", out)
	}
}

// Out-of-worktree (a temp dir is not a git work tree) caps the number of
// files scanned, so a low-hit search can't enumerate an entire home dir.
func TestGrep_OutOfWorktreeFileBudget(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 300; i++ {
		os.WriteFile(filepath.Join(tmp, "f"+itoa(i)+".txt"), []byte("nothing here\n"), 0o644)
	}
	out := runGrep(t, context.Background(), map[string]any{"pattern": "ZZZNOPE", "root": tmp})
	if !strings.Contains(out, "walk budget") {
		t.Errorf("expected the walk-budget notice for a 300-file out-of-worktree search, got: %q", out)
	}
}

// A cancelled context stops the walk promptly (Execute used to ignore ctx).
func TestGrep_HonorsContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 50; i++ {
		os.WriteFile(filepath.Join(tmp, "f"+itoa(i)+".txt"), []byte("hit\n"), 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	// Should return quickly (runGrep enforces the 10s ceiling) without panic.
	_ = runGrep(t, ctx, map[string]any{"pattern": "hit", "root": tmp})
}

// Files larger than grepMaxFileSize are skipped (line-scanning a multi-GB
// log/db is the other historical hang).
func TestGrep_SkipsOversizeFiles(t *testing.T) {
	tmp := t.TempDir()
	big := strings.Repeat("BIGMATCH padding line to add bytes\n", (grepMaxFileSize/35)+1000)
	os.WriteFile(filepath.Join(tmp, "huge.log"), []byte(big), 0o644)
	os.WriteFile(filepath.Join(tmp, "small.txt"), []byte("BIGMATCH small\n"), 0o644)

	out := runGrep(t, context.Background(), map[string]any{"pattern": "BIGMATCH", "root": tmp})
	if !strings.Contains(out, "small.txt") {
		t.Errorf("should match the small file, got: %q", out)
	}
	if strings.Contains(out, "huge.log") {
		t.Errorf("oversize file should be skipped, but it appeared in results: %q", out)
	}
}

// tiny int→string without importing strconv into the test (keeps it self-contained).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
