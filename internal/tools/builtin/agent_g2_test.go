package builtin

// agent_g2_test.go — locks Phase G.2 (per-invocation isolation +
// cwd, 2026-05-12). Mirrors claude-code's AgentTool's `isolation` /
// `cwd` schema fields.
//
// Five contracts pinned:
//
//   1. **Mutually exclusive**: `isolation` and `cwd` together → IsError.
//      Without this the model picks "one or the other" silently and
//      the operator can't tell which won.
//
//   2. **`isolation` enum**: any value other than "worktree" → IsError.
//      Schema accepts only "worktree" today (claude-code's "remote"
//      is intentionally out of scope for Phase G).
//
//   3. **`cwd` absolute-path requirement**: relative paths → IsError.
//      Parallel teammates would race on os.Getwd() otherwise.
//
//   4. **`cwd` directory existence + type check**: missing path or
//      non-dir → IsError with the underlying error visible.
//
//   5. **Backward compat**: no `isolation` and no `cwd` → no behavior
//      change, sub-agent inherits parent cwd, no worktree spawn.
//
// The actual worktree spawn path requires a real git repo with
// commits, which the unit-test environment doesn't guarantee — that
// case is reserved for the tmux 10+ rounds verification (manual,
// real repo).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestAgentExecute_IsolationAndCwdMutuallyExclusive — both fields
// set must reject. Critical contract: silently preferring one would
// surprise the caller.
func TestAgentExecute_IsolationAndCwdMutuallyExclusive(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":    "x",
		"isolation": "worktree",
		"cwd":       "/tmp",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("isolation+cwd together must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "mutually exclusive") {
		t.Errorf("error should name the conflict; got %q", res.Output)
	}
}

// TestAgentExecute_UnknownIsolation — `isolation: "remote"` (ant-only
// in claude-code) or any other string should reject with a hint
// naming the valid value.
func TestAgentExecute_UnknownIsolation(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	cases := []string{"remote", "container", "vm"}
	for _, iso := range cases {
		t.Run(iso, func(t *testing.T) {
			res, err := tool.Execute(context.Background(), map[string]any{
				"prompt":    "x",
				"isolation": iso,
			})
			if err != nil {
				t.Fatalf("Execute err: %v", err)
			}
			if !res.IsError {
				t.Errorf("isolation=%q must be IsError; got %+v", iso, res)
			}
			if !strings.Contains(res.Output, "only \"worktree\"") {
				t.Errorf("error should name the only valid value; got %q", res.Output)
			}
		})
	}
}

// TestAgentExecute_CwdRequiresAbsolutePath — relative paths reject.
// Parallel teammates can't safely resolve them.
func TestAgentExecute_CwdRequiresAbsolutePath(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "x",
		"cwd":    "relative/path",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("relative cwd must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "absolute path") {
		t.Errorf("error should name the abs-path requirement; got %q", res.Output)
	}
}

// TestAgentExecute_CwdNotADirectory — point cwd at a regular file →
// reject with "not a directory" message.
func TestAgentExecute_CwdNotADirectory(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(tmp, []byte("hi"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "x",
		"cwd":    tmp,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("cwd-points-at-file must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "not a directory") {
		t.Errorf("error should say 'not a directory'; got %q", res.Output)
	}
}

// TestAgentExecute_CwdMissingPath — non-existent path → IsError with
// the os.Stat error surfaced so the model sees "no such file or
// directory" rather than a vague "cwd invalid".
func TestAgentExecute_CwdMissingPath(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "x",
		"cwd":    "/this/path/does/not/exist/anywhere",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("missing cwd must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "/this/path/does/not/exist/anywhere") {
		t.Errorf("error should echo the bad path so model can correct; got %q", res.Output)
	}
}

// TestAgentExecute_CwdValidDirRunsClean — happy path: a real
// directory, no error, sub-agent runs to completion. The cwd ctx
// stamp itself is verified separately via CwdFromContext tests.
func TestAgentExecute_CwdValidDirRunsClean(t *testing.T) {
	tmp := t.TempDir() // already exists + is a directory

	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "x",
		"cwd":    tmp,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("valid cwd should run cleanly; got IsError: %s", res.Output)
	}
}

// TestAgentExecute_NoIsolationNoCwdBackwardCompat — the absence of
// both fields means inherit parent cwd, no worktree, behavior
// identical to pre-G.2. Critical: every pre-existing Agent caller
// must keep working without changes.
func TestAgentExecute_NoIsolationNoCwdBackwardCompat(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "go do a thing",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("no isolation + no cwd should run cleanly (backward compat); got IsError: %s", res.Output)
	}
	if !strings.Contains(res.Output, "sub-agent done") {
		t.Errorf("expected helloProvider's text; got %q", res.Output)
	}
}
