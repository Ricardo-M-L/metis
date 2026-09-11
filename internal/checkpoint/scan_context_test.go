package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestCheckpointContextCancelledBeforeInitIsRetryable(t *testing.T) {
	skipIfNoGit(t)
	m, _ := freshManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hash, err := m.SnapContext(ctx, "Edit", "cancel", "cancel"); hash != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Snap hash=%q err=%v", hash, err)
	}
	if _, err := os.Stat(m.shadowDir); !os.IsNotExist(err) {
		t.Fatalf("cancelled init wrote shadow state: %v", err)
	}
	if hash, err := m.SnapContext(context.Background(), "Edit", "retry", "retry"); hash == "" || err != nil {
		t.Fatalf("same manager retry hash=%q err=%v", hash, err)
	}
}

func TestCheckpointContextPartialInitCanRetry(t *testing.T) {
	skipIfNoGit(t)
	m, _ := freshManager(t)
	// git init may be interrupted after mkdir(.git), before HEAD/config exist.
	if err := os.MkdirAll(filepath.Join(m.shadowDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if hash, err := m.SnapContext(context.Background(), "Edit", "retry", "retry partial init"); hash == "" || err != nil {
		t.Fatalf("partial initialization poisoned manager: hash=%q err=%v", hash, err)
	}
}

func TestCheckpointContextsBoundLockWait(t *testing.T) {
	skipIfNoGit(t)
	m, _ := freshManager(t)
	before, err := m.Snap("Edit", "baseline", "baseline")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(context.Context) error{
		"snap":    func(ctx context.Context) error { _, err := m.SnapContext(ctx, "Edit", "locked", "locked"); return err },
		"delta":   func(ctx context.Context) error { _, err := m.ChangedPathsContext(ctx, before); return err },
		"capture": func(ctx context.Context) error { _, err := m.CapturePathStatesContext(ctx, []string{"x"}); return err },
		"record":  func(ctx context.Context) error { return m.RecordManagedPathsContext(ctx, []string{"x"}) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			m.mu.Lock()
			defer m.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lock wait err=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("checkpoint did not cancel a contended lock wait")
			}
		})
	}
}

func TestChangedPathsContextPreservesFormatsAndAttribution(t *testing.T) {
	skipIfNoGit(t)
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("GIT_DEFAULT_HASH", format)
			m, cwd := freshManager(t)
			write := func(name, body string) {
				t.Helper()
				path := filepath.Join(cwd, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"unchanged", "tab\tname", "new\nline", "deleted", "renamed", "executable"} {
				write(name, "before\x00\n")
			}
			before, err := m.Snap("Bash", "baseline", "baseline")
			if err != nil {
				t.Fatal(err)
			}
			if (format == "sha256" && len(before) != 64) || (format == "sha1" && len(before) != 40) {
				t.Fatalf("snapshot object format %s: hash=%q", format, before)
			}
			write("tab\tname", "after\x00\n")
			write("new\nline", "after\n")
			write("created", "new")
			write("node_modules/ignored", "skip")
			if err := os.Remove(filepath.Join(cwd, "deleted")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(cwd, "renamed"), filepath.Join(cwd, "destination")); err != nil {
				t.Fatal(err)
			}
			// Preserve the legacy content-only delta semantics. Execute-bit
			// fingerprints and restore safety are covered by their own tests.
			if err := os.Chmod(filepath.Join(cwd, "executable"), 0o700); err != nil {
				t.Fatal(err)
			}
			got, err := m.ChangedPathsContext(context.Background(), before)
			want := []string{"created", "deleted", "destination", "new\nline", "renamed", "tab\tname"}
			sort.Strings(want)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("delta got=%q want=%q err=%v", got, want, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if paths, err := m.ChangedPathsContext(ctx, before); paths != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled delta returned partial result: %v %v", paths, err)
			}
		})
	}
}

func TestSnapReconcilesUntrackedPartialMirror(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if _, err := m.Snap("Bash", "baseline", "baseline"); err != nil {
		t.Fatal(err)
	}
	// Model an interrupted copy before git add: this file was copied from the
	// live tree but remained untracked, and the user subsequently removed it.
	stale := filepath.Join(m.shadowDir, "copied-before-cancel.txt")
	if err := os.WriteFile(stale, []byte("must not resurrect"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "kept.txt"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := m.SnapContext(context.Background(), "Bash", "retry", "retry")
	if err != nil || hash == "" {
		t.Fatalf("retry hash=%q err=%v", hash, err)
	}
	paths, err := m.treePaths(hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, resurrected := paths["copied-before-cancel.txt"]; resurrected {
		t.Fatal("retry committed a stale untracked file from a partial mirror")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("retry did not reconcile partial mirror: %v", err)
	}
}
