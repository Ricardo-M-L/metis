package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPolicyDisabledSnapshotDoesNotPretendInitializationFailed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	shadow := filepath.Join(t.TempDir(), "shadow")
	m := NewManager("policy", home, shadow)
	_, err := m.BaselineContext(context.Background(), "Bash", "test", "home policy")
	if err == nil {
		t.Fatal("home policy unexpectedly allowed a snapshot")
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if strings.Contains(cause.Error(), "init error") || strings.Contains(cause.Error(), "initialization failed") {
			t.Fatalf("policy-only disabled snapshot falsely reports an initialization error: %v", cause)
		}
	}
	if _, statErr := os.Stat(shadow); !os.IsNotExist(statErr) {
		t.Fatalf("disabled snapshot touched shadow storage: %v", statErr)
	}
}

func TestFatalInitializationFailureRetainsOriginalCauseOnRetry(t *testing.T) {
	cwd := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block shadow storage"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager("fatal", cwd, blocker)
	_, first := m.BaselineContext(context.Background(), "Bash", "first", "first")
	var firstPath *os.PathError
	if !errors.As(first, &firstPath) {
		t.Fatalf("test requires a deterministic filesystem initialization failure, got %v", first)
	}
	_, second := m.BaselineContext(context.Background(), "Bash", "second", "retry")
	var secondPath *os.PathError
	if !errors.As(second, &secondPath) || secondPath.Path != firstPath.Path || !errors.Is(second, firstPath.Err) {
		t.Fatalf("disabled retry lost original initialization cause: first=%v second=%v", first, second)
	}
}

func TestDisabledReasonIdentifiesWorkspacePolicyAcrossOperations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, cwd := range []string{home, home + string(os.PathSeparator), "/", ""} {
		t.Run("workspace="+cwd, func(t *testing.T) {
			shadow := filepath.Join(t.TempDir(), "shadow")
			m := NewManager("policy", cwd, shadow)
			if !m.Disabled() || !m.DisabledByPolicy() || !errors.Is(m.DisabledReason(), ErrDisabledByPolicy) {
				t.Fatalf("workspace policy is not identifiable: disabled=%v reason=%v", m.Disabled(), m.DisabledReason())
			}
			calls := map[string]func() error{
				"snapshot": func() error { _, err := m.Snap("Bash", "policy", "policy"); return err },
				"baseline": func() error {
					_, err := m.BaselineContext(context.Background(), "Bash", "policy", "policy")
					return err
				},
				"delta":          func() error { _, err := m.ChangedPaths("deadbeef"); return err },
				"capture":        func() error { _, err := m.CapturePathStates([]string{"x"}); return err },
				"record":         func() error { return m.RecordManagedPath("x") },
				"restore":        func() error { return m.Restore("deadbeef") },
				"restore-paths":  func() error { return m.RestorePaths("deadbeef", []string{"x"}) },
				"restore-states": func() error { return m.RestorePathStates("deadbeef", map[string]string{}) },
				"diff":           func() error { _, err := m.Diff("deadbeef", ""); return err },
			}
			for name, call := range calls {
				if err := call(); !errors.Is(err, ErrDisabledByPolicy) {
					t.Errorf("%s discarded policy reason: %v", name, err)
				}
			}
			if _, err := os.Stat(shadow); !os.IsNotExist(err) {
				t.Fatalf("policy-disabled operation touched shadow storage: %v", err)
			}
		})
	}
}

func TestDisabledReasonPreservesFatalInitializationAndDoesNotLabelPolicy(t *testing.T) {
	m := NewManager("fatal", t.TempDir(), t.TempDir())
	if reason := m.DisabledReason(); reason != nil {
		t.Fatalf("new project manager already has disabled reason: %v", reason)
	}
	if err := os.WriteFile(m.shadowDir, []byte("cannot initialize here"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, first := m.Snap("Bash", "fatal", "fatal")
	var cause *os.PathError
	if !errors.As(first, &cause) {
		t.Fatalf("expected filesystem initialization cause: %v", first)
	}
	reason := m.DisabledReason()
	if !m.Disabled() || m.DisabledByPolicy() || reason == nil || errors.Is(reason, ErrDisabledByPolicy) || !errors.Is(reason, cause.Err) {
		t.Fatalf("fatal init was hidden or mislabeled as policy: %v", reason)
	}
	if !strings.Contains(reason.Error(), "initialization failed") {
		t.Fatalf("fatal init reason lacks context: %v", reason)
	}
	for name, call := range map[string]func() error{
		"snapshot":       func() error { _, err := m.Snap("Bash", "fatal", "again"); return err },
		"delta":          func() error { _, err := m.ChangedPaths("deadbeef"); return err },
		"capture":        func() error { _, err := m.CapturePathStates([]string{"x"}); return err },
		"record":         func() error { return m.RecordManagedPath("x") },
		"restore":        func() error { return m.Restore("deadbeef") },
		"restore-paths":  func() error { return m.RestorePaths("deadbeef", []string{"x"}) },
		"restore-states": func() error { return m.RestorePathStates("deadbeef", nil) },
		"diff":           func() error { _, err := m.Diff("deadbeef", ""); return err },
	} {
		if err := call(); !errors.Is(err, cause.Err) || errors.Is(err, ErrDisabledByPolicy) {
			t.Errorf("%s discarded fatal cause or mislabeled it as policy: %v", name, err)
		}
	}
}

func TestDisabledReasonCancellationRemainsRetryable(t *testing.T) {
	skipIfNoGit(t)
	m, _ := freshManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.BaselineContext(ctx, "Bash", "cancel", "cancel"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot returned %v", err)
	}
	if m.Disabled() || m.DisabledReason() != nil {
		t.Fatalf("cancellation latched a disabled reason: %v", m.DisabledReason())
	}
	if hash, err := m.BaselineContext(context.Background(), "Bash", "retry", "retry"); err != nil || hash == "" {
		t.Fatalf("same manager could not retry: hash=%q err=%v", hash, err)
	}
	if m.DisabledReason() != nil {
		t.Fatalf("successful retry has a disabled reason: %v", m.DisabledReason())
	}
}

func TestDisabledByPolicyDoesNotWaitForSnapshotMutex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, cwd := range []string{home, t.TempDir()} {
		m := NewManager("policy-lock", cwd, t.TempDir())
		want := m.DisabledByPolicy()
		m.mu.Lock()
		done := make(chan bool, 1)
		go func() { done <- m.DisabledByPolicy() }()
		select {
		case got := <-done:
			m.mu.Unlock()
			if got != want {
				t.Fatalf("policy changed while scanning: got=%v want=%v", got, want)
			}
		case <-time.After(time.Second):
			m.mu.Unlock()
			<-done
			t.Fatal("policy fast path waited for the active snapshot mutex")
		}
	}
}

func TestUnsafeCheckpointRootsUsePlatformPathSemantics(t *testing.T) {
	roots := []string{"/", string(filepath.Separator)}
	projects := []string{".", "project", filepath.Join(t.TempDir(), "project")}
	if runtime.GOOS == "windows" {
		roots = append(roots, `C:\`, `D:/`, `\\server\share\`, `\\server\share`)
		projects = append(projects, `C:\project`, `C:project`, `\\server\share\project`)
	}
	for _, root := range roots {
		if !isUnsafeCheckpointRoot(root) {
			t.Errorf("filesystem root was allowed: %q", root)
		}
	}
	for _, project := range projects {
		if isUnsafeCheckpointRoot(project) {
			t.Errorf("project was mistaken for a filesystem root: %q", project)
		}
	}
}
