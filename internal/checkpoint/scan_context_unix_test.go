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

func TestSnapContextCancelGitWriteReleasesLockAndCanRetry(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if _, err := m.Snap("Bash", "baseline", "empty baseline"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "filter-started")
	filter := filepath.Join(t.TempDir(), "filter.sh")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nif [ ! -e " + quote(marker) + " ]; then\n  : > " + quote(marker) + "\n  sleep 30\nfi\ncat\n"
	if err := os.WriteFile(filter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.git("config", "filter.cancel-test.clean", quote(filter)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".gitattributes"), []byte("*.txt filter=cancel-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "file.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.SnapContext(ctx, "Bash", "interrupted", "interrupted add"); done <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
waitForFilter:
	for {
		select {
		case err := <-done:
			t.Fatalf("scan returned before filter: %v", err)
		case <-deadline.C:
			t.Fatal("Git filter did not start")
		case <-ticker.C:
			if _, err := os.Stat(marker); err == nil {
				break waitForFilter
			}
		}
	}
	if _, err := os.Stat(filepath.Join(m.shadowDir, ".git", "index.lock")); err != nil {
		t.Fatalf("Git add had no lock at barrier: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Git filter was not joined")
	}
	if _, err := os.Stat(filepath.Join(m.shadowDir, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("cancelled Git left index.lock: %v", err)
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer retryCancel()
	if hash, err := m.SnapContext(retryCtx, "Bash", "retry", "retry after cancel"); err != nil || hash == "" {
		t.Fatalf("retry hash=%q err=%v", hash, err)
	}
}
