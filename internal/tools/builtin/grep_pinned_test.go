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

func TestGrepRejectsRootSymlinkSwapAfterPermission(t *testing.T) {
	dir := t.TempDir()
	safeRoot := filepath.Join(dir, "safe")
	attackerRoot := filepath.Join(dir, "attacker")
	for _, root := range []string{safeRoot, attackerRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWritePinnedTestFile(t, filepath.Join(safeRoot, "notes.txt"), "needle safe\n")
	mustWritePinnedTestFile(t, filepath.Join(attackerRoot, "notes.txt"), "needle attacker must not leak\n")
	link := filepath.Join(dir, "root")
	if err := os.Symlink(safeRoot, link); err != nil {
		t.Fatal(err)
	}
	tool := NewGrep(permission.New(permission.ModeBypassPermissions))
	in := map[string]any{"root": link, "pattern": "needle"}
	ctx := tools.WithInvocationID(context.Background(), "grep-swap")
	if got, _ := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %q, want allow", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attackerRoot, link); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || strings.Contains(result.Output, "attacker must not leak") {
		t.Fatalf("result = %+v, want secret-free fail-closed result", result)
	}
}

func TestGrepRejectsRootSymlinkSwapAfterOpen(t *testing.T) {
	dir := t.TempDir()
	safeRoot := filepath.Join(dir, "safe")
	attackerRoot := filepath.Join(dir, "attacker")
	for _, root := range []string{safeRoot, attackerRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWritePinnedTestFile(t, filepath.Join(safeRoot, "notes.txt"), "needle safe\n")
	mustWritePinnedTestFile(t, filepath.Join(attackerRoot, "notes.txt"), "needle attacker must not leak\n")
	link := filepath.Join(dir, "root")
	if err := os.Symlink(safeRoot, link); err != nil {
		t.Fatal(err)
	}
	tool := NewGrep(permission.New(permission.ModeBypassPermissions))
	tool.afterRootOpen = func() {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attackerRoot, link); err != nil {
			t.Fatal(err)
		}
	}
	result, err := tool.Execute(context.Background(), map[string]any{"root": link, "pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || strings.Contains(result.Output, "attacker must not leak") {
		t.Fatalf("result = %+v, want secret-free fail-closed result", result)
	}
}

func TestGrepRejectsFileSwapAfterOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	mustWritePinnedTestFile(t, path, "needle safe\n")
	tool := NewGrep(permission.New(permission.ModeBypassPermissions))
	tool.afterFileOpen = func(openedPath string) {
		if openedPath != path {
			return
		}
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		mustWritePinnedTestFile(t, path, "needle attacker must not leak\n")
	}
	result, err := tool.Execute(context.Background(), map[string]any{"root": root, "pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || strings.Contains(result.Output, "attacker must not leak") {
		t.Fatalf("result = %+v, want secret-free fail-closed result", result)
	}
}

func TestGrepDeniedBindingCannotAuthorizeLaterInvocation(t *testing.T) {
	root := t.TempDir()
	mustWritePinnedTestFile(t, filepath.Join(root, "notes.txt"), "needle private\n")
	gate := permission.New(permission.ModeDefault)
	gate.AppendRules(permission.Rule{Tool: "Grep", Match: "needle", Verb: permission.DecisionAsk, Source: "test:ask"})
	tool := NewGrep(gate)
	in := map[string]any{"root": root, "pattern": "needle"}
	deniedCtx := tools.WithInvocationID(context.Background(), "grep-ask-denied")
	if got, _ := tool.CanUse(deniedCtx, in); got != tools.PermissionAsk {
		t.Fatalf("CanUse = %v, want ASK", got)
	}
	laterCtx := tools.WithInvocationID(context.Background(), "grep-later")
	result, err := tool.Execute(laterCtx, in)
	if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "needle private") {
		t.Fatalf("later Execute = %+v, %v; want missing-binding refusal", result, err)
	}
}

func TestGrepRedactsGenericCredentialAssignment(t *testing.T) {
	root := t.TempDir()
	mustWritePinnedTestFile(t, filepath.Join(root, "notes.txt"), "CUSTOM_API_KEY=plain-looking-secret\nordinary=kept\n")
	result, err := NewGrep(nil).Execute(context.Background(), map[string]any{"root": root, "pattern": "="})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, "plain-looking-secret") ||
		!strings.Contains(result.Output, "CUSTOM_API_KEY=[REDACTED]") || !strings.Contains(result.Output, "ordinary=kept") {
		t.Fatalf("result = %+v, want generic assignment redacted", result)
	}
}
