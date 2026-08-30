package builtin

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestRunCodeConcurrencyIsExclusive(t *testing.T) {
	if got := (RunCode{}).Concurrency(nil); got != tools.ConcurrencyExclusive {
		t.Fatalf("RunCode.Concurrency = %v, want ConcurrencyExclusive", got)
	}
}

func TestStopRunCodeProcessWaitIsBounded(t *testing.T) {
	done := make(chan error)
	start := time.Now()
	stopRunCodeProcess((*exec.Cmd)(nil), done, 20*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed < 15*time.Millisecond {
		t.Fatalf("stopRunCodeProcess returned too early after %s", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("stopRunCodeProcess blocked for %s", elapsed)
	}
}

func TestRunCodeCreatesSourceUnderManagerOwnedTempRoot(t *testing.T) {
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:     string(sandbox.ModeOff),
		TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	runner := NewRunCodeWithSandbox(nil, manager)
	result, err := runner.Execute(context.Background(), map[string]any{
		"language": "bash",
		"code":     `pwd; printf '\n%s\n' "$TMPDIR"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError {
		t.Fatalf("RunCode result = %#v", result)
	}
	lines := strings.Fields(result.Output)
	if len(lines) != 2 {
		t.Fatalf("RunCode output = %q, want cwd and TMPDIR", result.Output)
	}
	got := filepath.Clean(lines[0])
	rel, err := filepath.Rel(manager.TempDir(), got)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("RunCode cwd = %q, want descendant of manager temp root %q", got, manager.TempDir())
	}
	if gotTMP := filepath.Clean(lines[1]); gotTMP != manager.TempDir() {
		t.Fatalf("RunCode TMPDIR = %q, want manager temp root %q", gotTMP, manager.TempDir())
	}
}
