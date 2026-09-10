package builtin

// agent_g2_test.go — locks Phase G.2 (per-invocation isolation +
// cwd, 2026-05-12). Mirrors claude-code's AgentTool's `isolation` /
// `cwd` schema fields.
//
// Five contracts pinned:
//
//   1. **Explicit repository**: `isolation: "worktree"` and `cwd`
//      together spawn from the repository containing cwd.
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
import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentExecute_IsolationWithCwdUsesThatRepository(t *testing.T) {
	repo := t.TempDir()
	runAgentTestGit(t, repo, "init", "--quiet")
	runAgentTestGit(t, repo, "config", "user.name", "Metis Agent Test")
	runAgentTestGit(t, repo, "config", "user.email", "metis-agent-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runAgentTestGit(t, repo, "add", "README.md")
	runAgentTestGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
	t.Setenv("METIS_HOME", filepath.Join(t.TempDir(), "metis-home"))

	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":    "x",
		"isolation": "worktree",
		"cwd":       repo,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Fatalf("isolation+cwd should spawn from cwd's repository: %s", res.Output)
	}
}

func TestAgentExecute_WorktreeOutsideGitExplainsHowToRecover(t *testing.T) {
	nonRepo := t.TempDir()
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":    "x",
		"isolation": "worktree",
		"cwd":       nonRepo,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("non-git worktree request must fail: %+v", res)
	}
	for _, want := range []string{"existing Git repository", "omit isolation", "Do not repeat"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("recovery error missing %q: %s", want, res.Output)
		}
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
