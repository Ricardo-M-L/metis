package builtin

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestGitSandboxManagerInjectionAndWrapFailure(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewGitWithSandbox(permission.New(permission.ModeBypassPermissions), manager)
	if tool.SandboxManager() != manager {
		t.Fatal("NewGitWithSandbox did not retain Manager")
	}
	if NewGit(permission.New(permission.ModeBypassPermissions)).WithSandbox(manager).SandboxManager() != manager {
		t.Fatal("Git.WithSandbox did not retain Manager")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := tool.Execute(context.Background(), map[string]any{"args": "status"})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "Git: sandbox wrap failed") || !strings.Contains(res.Output, sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("got %+v, want explicit sandbox Wrap failure", res)
	}
}

func TestGitSandboxAutoAllowOnlyReplacesAsk(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeAutoAllow))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	in := map[string]any{"args": "status"}

	ask := NewGitWithSandbox(permission.New(permission.ModeDefault), manager)
	if got, _ := ask.CanUse(context.Background(), in); got != tools.PermissionAllow {
		t.Fatalf("ordinary Ask + auto-allow = %v, want allow", got)
	}
	plan := NewGitWithSandbox(permission.New(permission.ModePlan), manager)
	if got, _ := plan.CanUse(context.Background(), in); got != tools.PermissionDeny {
		t.Fatalf("Plan + auto-allow = %v, want deny", got)
	}
	deniedGate := permission.New(permission.ModeDefault)
	deniedGate.AppendRules(permission.Rule{Tool: "Git", Verb: permission.DecisionDeny, Source: "test"})
	denied := NewGitWithSandbox(deniedGate, manager)
	if got, _ := denied.CanUse(context.Background(), in); got != tools.PermissionDeny {
		t.Fatalf("explicit deny + auto-allow = %v, want deny", got)
	}
}

func TestSetGitTempEnvReplacesHostTemp(t *testing.T) {
	env := setGitTempEnv([]string{"PATH=/bin", "TMPDIR=/host", "TEMP=/host", "TMP=/host"}, "/private/runtime-temp")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"TMPDIR=/private/runtime-temp", "TMP=/private/runtime-temp", "TEMP=/private/runtime-temp"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	if strings.Contains(joined, "=/host") {
		t.Fatalf("host temp leaked through replacement: %v", env)
	}
}

func TestGitUsesAgentContextCwdWhenInputOmitsCwd(t *testing.T) {
	cwd := t.TempDir()
	if output, err := exec.Command("git", "init", "--quiet", cwd).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	tool := NewGit(permission.New(permission.ModeBypassPermissions))
	res, err := tool.Execute(agent.WithCwd(context.Background(), cwd), map[string]any{"args": "rev-parse --show-toplevel"})
	if err != nil || res.IsError {
		t.Fatalf("Git cwd probe failed: err=%v result=%+v", err, res)
	}
	want, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.Output))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Git cwd=%q, want context cwd %q", got, want)
	}
}
