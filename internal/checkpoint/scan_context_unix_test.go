//go:build !windows

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This is a process-count regression, not a machine-speed assertion. A Bash
// result used to wait for one `git show` process per checkpointed file even
// when the shell command had already finished and changed nothing.
func TestChangedPathsUsesBoundedGitProcesses(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	for i := 0; i < 64; i++ {
		if err := os.WriteFile(filepath.Join(cwd, fmt.Sprintf("file-%03d.txt", i)), []byte("unchanged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := m.Snap("Bash", "before", "before command")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "git-calls")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> " + quote(log) + "\nexec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	changed, err := m.ChangedPaths(before)
	if err != nil || len(changed) != 0 {
		t.Fatalf("unchanged tree: paths=%v err=%v", changed, err)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Fields(string(commands)); len(calls) > 2 {
		t.Fatalf("unchanged 64-file scan launched %d Git processes; want at most 2 (no per-file git show)", len(calls))
	}
}

func TestInventoryFileReplacedByFIFODoesNotBlockCancellation(t *testing.T) {
	m, cwd := freshManager(t)
	path := filepath.Join(cwd, "replaced")
	if err := os.WriteFile(path, []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	tree, err := m.inventoryTree(context.Background(), root, newScanObserver(context.Background()))
	if err != nil || len(tree.files) != 1 {
		t.Fatalf("inventory files=%d err=%v", len(tree.files), err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := processLiveFiles(ctx, tree.files, newScanObserver(ctx), func(file liveFile) fileResult {
			opened <- struct{}{}
			_, err := readLiveFile(root, file)
			return fileResult{err: err}
		})
		done <- err
	}()
	<-opened
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO replacement was treated as regular contents")
		}
	case <-time.After(time.Second):
		// Unstick a buggy blocking reader so a failing test does not leave a
		// goroutine/file descriptor behind or hang the full suite.
		if fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(fd)
		}
		<-done
		t.Fatal("FIFO replacement left a blocked worker after cancellation")
	}
}

func TestBaselineDoesNotExecuteSourceCleanFilter(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if _, err := m.BaselineContext(context.Background(), "Bash", "empty", "initialize"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "filter-executed")
	filter := filepath.Join(t.TempDir(), "filter.sh")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if err := os.WriteFile(filter, []byte("#!/bin/sh\n: > "+quote(marker)+"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.git("config", "filter.must-not-run.clean", quote(filter)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".gitattributes"), []byte("*.txt filter=must-not-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "original.txt"), []byte("raw bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BaselineContext(context.Background(), "Bash", "source-filter", "source-filter"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("snapshot executed a source-controlled clean filter: %v", err)
	}
}

func TestSnapContextCancelGitWriteReleasesLockAndCanRetry(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if _, err := m.Snap("Bash", "baseline", "empty baseline"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-started")
	hookDir := t.TempDir()
	hook := filepath.Join(hookDir, "reference-transaction")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\nif [ ! -e " + quote(marker) + " ]; then\n  : > " + quote(marker) + "\n  sleep 30\nfi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.git("config", "core.hooksPath", hookDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "file.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.SnapContext(ctx, "Bash", "interrupted", "interrupted commit"); done <- err }()
	// Starting git commit can be delayed by other packages in a full-suite run;
	// the marker, not elapsed startup time, is the barrier this test needs.
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waitForHook:
	for {
		select {
		case err := <-done:
			t.Fatalf("scan returned before hook: %v", err)
		case <-deadline.C:
			t.Fatal("Git hook did not start")
		case <-ticker.C:
			if _, err := os.Stat(marker); err == nil {
				break waitForHook
			}
		}
	}
	ref, err := m.gitOutput("symbolic-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(m.shadowDir, ".git", filepath.FromSlash(ref)+".lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("Git commit had no lock at barrier: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Git hook was not joined")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("cancelled Git left a reference lock: %v", err)
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer retryCancel()
	if hash, err := m.SnapContext(retryCtx, "Bash", "retry", "retry after cancel"); err != nil || hash == "" {
		t.Fatalf("retry hash=%q err=%v", hash, err)
	}
}
