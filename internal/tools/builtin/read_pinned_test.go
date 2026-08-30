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

func TestReadRejectsSymlinkSwapAfterPermission(t *testing.T) {
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe.txt")
	attacker := filepath.Join(dir, "attacker.txt")
	link := filepath.Join(dir, "input.txt")
	mustWritePinnedTestFile(t, safe, "safe content\n")
	mustWritePinnedTestFile(t, attacker, "attacker content must not leak\n")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}

	tool := Read{
		gate:       permission.New(permission.ModeBypassPermissions),
		authorizer: newReadPathAuthorizer(),
	}
	ctx := tools.WithInvocationID(context.Background(), "read-swap")
	if got, _ := tool.CanUse(ctx, map[string]any{"path": link}); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %q, want allow", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(ctx, map[string]any{"path": link})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || strings.Contains(result.Output, "attacker content") {
		t.Fatalf("result = %+v, want secret-free fail-closed result", result)
	}
}

func TestReadRejectsSymlinkSwapAfterOpen(t *testing.T) {
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe.txt")
	attacker := filepath.Join(dir, "attacker.txt")
	link := filepath.Join(dir, "input.txt")
	mustWritePinnedTestFile(t, safe, "safe content\n")
	mustWritePinnedTestFile(t, attacker, "attacker content must not leak\n")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}

	tool := Read{
		gate: permission.New(permission.ModeBypassPermissions),
		afterOpen: func() {
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(attacker, link); err != nil {
				t.Fatal(err)
			}
		},
	}
	result, err := tool.Execute(context.Background(), map[string]any{"path": link})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || strings.Contains(result.Output, "attacker content") {
		t.Fatalf("result = %+v, want secret-free fail-closed result", result)
	}
}

func TestReadDeniedBindingCannotAuthorizeLaterInvocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	mustWritePinnedTestFile(t, path, "private content\n")
	gate := permission.New(permission.ModeDefault)
	gate.AppendRules(permission.Rule{Tool: "Read", Match: path, Verb: permission.DecisionAsk, Source: "test:ask"})
	tool := Read{gate: gate, authorizer: newReadPathAuthorizer()}
	deniedCtx := tools.WithInvocationID(context.Background(), "ask-that-user-denied")
	if got, _ := tool.CanUse(deniedCtx, map[string]any{"path": path}); got != tools.PermissionAsk {
		t.Fatalf("CanUse = %v, want ASK", got)
	}
	// Simulate the dispatcher/user denying the ASK: Execute is never called for
	// that invocation. A new invocation without its own CanUse binding must not
	// consume the abandoned approval by path or FIFO order.
	laterCtx := tools.WithInvocationID(context.Background(), "later-call")
	result, err := tool.Execute(laterCtx, map[string]any{"path": path})
	if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "private content") {
		t.Fatalf("later Execute = %+v, %v; want missing-binding refusal", result, err)
	}
}

func TestReadRedactsGenericCredentialAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	mustWritePinnedTestFile(t, path, "CUSTOM_API_KEY=plain-looking-secret\nordinary=kept\n")
	result, err := (Read{}).Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, "plain-looking-secret") ||
		!strings.Contains(result.Output, "CUSTOM_API_KEY=[REDACTED]") || !strings.Contains(result.Output, "ordinary=kept") {
		t.Fatalf("result = %+v, want generic assignment redacted", result)
	}
}

func mustWritePinnedTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
