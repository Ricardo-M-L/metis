package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentPermissionBoundToolOuterAskPreparesReadBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("bound read content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	childGate := permission.New(permission.ModeDefault)
	childGate.AppendRules(permission.Rule{Tool: "Read", Match: path, Verb: permission.DecisionAsk, Source: "test:child-ask"})
	inner := Read{gate: permission.New(permission.ModeBypassPermissions), authorizer: newReadPathAuthorizer()}
	tool := agentPermissionBoundTool{inner: inner, gate: childGate}
	ctx := tools.WithInvocationID(context.Background(), "child-read")
	in := map[string]any{"path": path}

	if got, _ := tool.CanUse(ctx, in); got != tools.PermissionAsk {
		t.Fatalf("CanUse = %v, want ASK", got)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "bound read content") {
		t.Fatalf("approved Read Execute = %#v, %v", result, err)
	}
}

func TestAgentPermissionBoundToolOuterAskPreparesEditBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := Edit{
		gate:       permission.New(permission.ModeBypassPermissions),
		authorizer: newInvocationAuthorizer[approvedExistingPath](),
	}
	tool := agentPermissionBoundTool{inner: inner, gate: permission.New(permission.ModeDefault)}
	ctx := tools.WithInvocationID(context.Background(), "child-edit")
	in := map[string]any{"path": path, "old": "before", "new": "after"}

	if got, _ := tool.CanUse(ctx, in); got != tools.PermissionAsk {
		t.Fatalf("CanUse = %v, want ASK", got)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("approved Edit Execute = %#v, %v", result, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "after\n" {
		t.Fatalf("edited content = %q, %v", got, err)
	}
}
