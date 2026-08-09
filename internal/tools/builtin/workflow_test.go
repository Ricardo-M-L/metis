package builtin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

func inlineSteps(steps ...[2]string) []any {
	out := make([]any, 0, len(steps))
	for _, s := range steps {
		out = append(out, map[string]any{"name": s[0], "command": s[1]})
	}
	return out
}

func TestWorkflowTool_RunInline(t *testing.T) {
	w := NewWorkflow(permission.New(permission.ModeBypass), nil)
	res, err := w.Execute(context.Background(), map[string]any{
		"operation": "run",
		"steps":     inlineSteps([2]string{"greet", "echo hello-wf"}, [2]string{"second", "echo two"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	for _, want := range []string{"[ok] greet", "hello-wf", "[ok] second"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q:\n%s", want, res.Output)
		}
	}
}

func TestWorkflowTool_RunInlineStopsAndFlagsError(t *testing.T) {
	w := NewWorkflow(permission.New(permission.ModeBypass), nil)
	res, _ := w.Execute(context.Background(), map[string]any{
		"operation": "run",
		"steps":     inlineSteps([2]string{"ok", "echo fine"}, [2]string{"boom", "exit 2"}, [2]string{"after", "echo nope"}),
	})
	if !res.IsError {
		t.Error("a failing step should mark the result IsError")
	}
	if !strings.Contains(res.Output, "[failed] boom") {
		t.Errorf("missing failed marker:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "[skipped] after") {
		t.Errorf("step after a failure should be skipped:\n%s", res.Output)
	}
}

func TestWorkflowTool_SaveRunNamedList(t *testing.T) {
	store := workflow.NewStore(t.TempDir())
	w := NewWorkflow(permission.New(permission.ModeBypass), store)

	save, err := w.Execute(context.Background(), map[string]any{
		"operation": "save",
		"name":      "greet",
		"steps":     inlineSteps([2]string{"hi", "echo saved-and-run"}),
	})
	if err != nil || save.IsError {
		t.Fatalf("save failed: %v / %s", err, save.Output)
	}

	run, _ := w.Execute(context.Background(), map[string]any{"operation": "run_named", "name": "greet"})
	if run.IsError || !strings.Contains(run.Output, "saved-and-run") {
		t.Errorf("run_named: %s", run.Output)
	}

	list, _ := w.Execute(context.Background(), map[string]any{"operation": "list"})
	if !strings.Contains(list.Output, "greet") {
		t.Errorf("list missing saved workflow: %s", list.Output)
	}
}

// Every step command is gated like a Bash call: a deny rule on a step
// command must deny the whole workflow before anything runs.
func TestWorkflowTool_PermissionGatesEveryStep(t *testing.T) {
	g := permission.New(permission.ModeAsk)
	g.AppendRules(permission.Rule{Tool: "Bash", Match: "curl:*", Verb: permission.DecisionDeny, Source: "config:deny"})
	w := NewWorkflow(g, nil)

	in := map[string]any{
		"operation": "run",
		"steps":     inlineSteps([2]string{"ok", "echo fine"}, [2]string{"bad", "curl http://evil"}),
	}
	perm, _ := w.CanUse(context.Background(), in)
	if perm != tools.PermissionDeny {
		t.Errorf("a denied step command must deny the workflow; got %v", perm)
	}
}

func TestWorkflowTool_NamedUnavailableWithoutStore(t *testing.T) {
	w := NewWorkflow(permission.New(permission.ModeBypass), nil)
	res, _ := w.Execute(context.Background(), map[string]any{"operation": "run_named", "name": "x"})
	if !res.IsError || !strings.Contains(res.Output, "not available") {
		t.Errorf("run_named without a store should report unavailable; got %s", res.Output)
	}
}

func TestWorkflowTool_SandboxManagerInjectionAndWrapFailure(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorkflowWithSandbox(permission.New(permission.ModeBypass), nil, manager)
	if w.SandboxManager() != manager {
		t.Fatal("NewWorkflowWithSandbox did not retain Manager")
	}
	if NewWorkflow(permission.New(permission.ModeBypass), nil).WithSandbox(manager).SandboxManager() != manager {
		t.Fatal("Workflow.WithSandbox did not retain Manager")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := w.Execute(context.Background(), map[string]any{
		"operation": "run",
		"steps":     inlineSteps([2]string{"blocked", "echo should-not-run"}),
	})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "sandbox wrap failed") || !strings.Contains(res.Output, sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("got %+v, want explicit sandbox Wrap failure", res)
	}
}

func TestWorkflowTool_SandboxAutoAllowOnlyReplacesAsk(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeAutoAllow))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	in := map[string]any{
		"operation": "run",
		"steps":     inlineSteps([2]string{"write", "touch workflow-auto-allow"}),
	}

	ask := NewWorkflowWithSandbox(permission.New(permission.ModeDefault), nil, manager)
	if got, _ := ask.CanUse(context.Background(), in); got != tools.PermissionAllow {
		t.Fatalf("ordinary Ask + auto-allow = %v, want allow", got)
	}
	plan := NewWorkflowWithSandbox(permission.New(permission.ModePlan), nil, manager)
	if got, _ := plan.CanUse(context.Background(), in); got != tools.PermissionDeny {
		t.Fatalf("Plan + auto-allow = %v, want deny", got)
	}
	deniedGate := permission.New(permission.ModeDefault)
	deniedGate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionDeny, Source: "test"})
	denied := NewWorkflowWithSandbox(deniedGate, nil, manager)
	if got, _ := denied.CanUse(context.Background(), in); got != tools.PermissionDeny {
		t.Fatalf("explicit deny + auto-allow = %v, want deny", got)
	}
}

func TestWorkflowTool_UsesAgentContextCwd(t *testing.T) {
	cwd := t.TempDir()
	w := NewWorkflow(permission.New(permission.ModeBypass), nil)
	res, err := w.Execute(agent.WithCwd(context.Background(), cwd), map[string]any{
		"operation": "run",
		"steps":     inlineSteps([2]string{"cwd", "pwd"}),
	})
	if err != nil || res.IsError {
		t.Fatalf("workflow cwd probe failed: err=%v result=%+v", err, res)
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, resolved) && !strings.Contains(res.Output, cwd) {
		t.Fatalf("workflow ignored context cwd %q: %s", cwd, res.Output)
	}
}
