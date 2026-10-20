package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

func TestWorkflowLearningToolPinnedRecipeAndSandbox(t *testing.T) {
	if d := sandbox.Doctor(); !d.Available {
		t.Skip(d.Err)
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	home := filepath.Join(root, "metis")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	store := workflow.NewStore(filepath.Join(home, "workflows"))
	wf := workflow.Workflow{Name: "arith", Steps: []workflow.Step{{Name: "write", Command: `printf '{"answer":%s}\n' "$((17 * 19))" > answer.json`}, {Name: "read", Command: "cat answer.json"}}}
	if err := store.Save(wf); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("arith")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_LEARNING_WORKFLOW_NAME", "arith")
	t.Setenv("METIS_LEARNING_WORKFLOW_VERSION", loaded.VersionSHA256)
	t.Setenv("METIS_LEARNING_WORKFLOW_CWD", cwd)
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "permissions", Network: sandbox.NetworkBlock, MetisHome: home})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	tool := NewWorkflowWithSandbox(permission.New(permission.ModeBypass), store, manager)
	ctx := agent.WithCwd(context.Background(), cwd)
	for _, in := range []map[string]any{{"operation": "run", "steps": inlineSteps([2]string{"evil", "echo fake"})}, {"operation": "save", "name": "arith", "steps": inlineSteps([2]string{"evil", "echo fake"})}, {"operation": "list"}, {"operation": "run_named", "name": "other"}} {
		if got, _ := tool.CanUse(ctx, in); got != tools.PermissionDeny {
			t.Fatalf("evaluation allowed %+v", in)
		}
		res, err := tool.Execute(ctx, in)
		if err != nil || !res.IsError {
			t.Fatalf("direct Execute allowed %+v: %+v %v", in, res, err)
		}
	}
	res, err := tool.Execute(ctx, map[string]any{"operation": "run_named", "name": "arith"})
	if err != nil || res.IsError {
		t.Fatalf("pinned recipe failed: %+v %v", res, err)
	}
	var record workflow.RunRecord
	if err := json.Unmarshal([]byte(res.Output), &record); err != nil {
		t.Fatal(err)
	}
	if record.RecipeSHA256 != loaded.VersionSHA256 || len(record.Steps) != 2 || strings.TrimSpace(record.Steps[1].Output) != `{"answer":323}` {
		t.Fatalf("missing actual output/version: %+v", record)
	}
	badCtx := agent.WithCwd(context.Background(), root)
	if res, _ := tool.Execute(badCtx, map[string]any{"operation": "run_named", "name": "arith"}); !res.IsError {
		t.Fatal("cwd substitution allowed")
	}
	t.Setenv("METIS_LEARNING_WORKFLOW_VERSION", strings.Repeat("b", 64))
	if res, _ := tool.Execute(ctx, map[string]any{"operation": "run_named", "name": "arith"}); !res.IsError {
		t.Fatal("version substitution allowed")
	}
}

func TestWorkflowNamedInvocationCannotBorrowEarlierVersionPermission(t *testing.T) {
	store := workflow.NewStore(t.TempDir())
	initial := workflow.Workflow{Name: "versioned", Steps: []workflow.Step{{Name: "answer", Command: "echo old"}}}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	tool := NewWorkflow(permission.New(permission.ModeBypass), store)
	in := map[string]any{"operation": "run_named", "name": "versioned"}
	ctx := tools.WithInvocationID(context.Background(), "first-call")
	if p, _ := tool.CanUse(ctx, in); p != tools.PermissionAllow {
		t.Fatal("baseline not authorized")
	}
	if err := store.Save(workflow.Workflow{Name: "versioned", Steps: []workflow.Step{{Name: "answer", Command: "echo replacement"}}}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(ctx, in)
	if err != nil || !res.IsError || !strings.Contains(res.Output, "invocation permission binding") {
		t.Fatalf("stale permission accepted new recipe: %+v %v", res, err)
	}
	// A different call cannot consume the first call's permission either.
	other := tools.WithInvocationID(context.Background(), "second-call")
	if res, _ := tool.Execute(other, in); !res.IsError {
		t.Fatal("different invocation borrowed permission")
	}
	if p, _ := tool.CanUse(other, in); p != tools.PermissionAllow {
		t.Fatal("new version not freshly authorized")
	}
	if res, _ := tool.Execute(other, in); res.IsError || !strings.Contains(res.Output, "replacement") {
		t.Fatalf("fresh invocation failed: %+v", res)
	}
	if res, _ := tool.Execute(other, in); !res.IsError {
		t.Fatal("one-shot recipe permission was reused")
	}
}
