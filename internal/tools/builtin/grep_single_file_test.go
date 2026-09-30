package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestGrepSingleFileRoot(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "authorized"}[authorized], func(t *testing.T) {
			root := t.TempDir()
			mustWritePinnedTestFile(t, filepath.Join(root, "target.go"), "needle first\nother\nneedle second\nneedle third\n")
			mustWritePinnedTestFile(t, filepath.Join(root, "sibling.go"), "needle sibling must not appear\n")
			tool := NewGrep(permission.New(permission.ModeBypassPermissions))
			ctx := agent.WithCwd(context.Background(), root)
			in := map[string]any{"root": "target.go", "pattern": "needle", "glob": "*.go", "offset": 1, "max": 1}
			if authorized {
				ctx = tools.WithInvocationID(ctx, "single-file")
				if got, reason := tool.CanUse(ctx, in); got != tools.PermissionAllow {
					t.Fatalf("CanUse = %v (%s), want allow", got, reason)
				}
			}
			result, err := tool.Execute(ctx, in)
			if err != nil || result == nil || result.IsError ||
				!strings.Contains(result.Output, "target.go:3:needle second") ||
				!strings.Contains(result.Output, "offset=2") ||
				strings.Contains(result.Output, "needle first") || strings.Contains(result.Output, "needle third") ||
				strings.Contains(result.Output, "sibling") {
				t.Fatalf("single-file paginated result = %+v, %v", result, err)
			}
		})
	}
}

func TestGrepSingleFileRejectsPathChanges(t *testing.T) {
	for _, when := range []string{"after_permission", "after_root_open", "after_file_open"} {
		t.Run(when, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "target.go")
			mustWritePinnedTestFile(t, path, "needle approved\n")
			tool := NewGrep(permission.New(permission.ModeBypassPermissions))
			ctx := tools.WithInvocationID(context.Background(), "single-file-swap")
			in := map[string]any{"root": path, "pattern": "needle"}
			if got, reason := tool.CanUse(ctx, in); got != tools.PermissionAllow {
				t.Fatalf("CanUse = %v (%s), want allow", got, reason)
			}
			swap := func() {
				if err := os.Rename(path, path+".approved"); err != nil {
					t.Fatal(err)
				}
				mustWritePinnedTestFile(t, path, "needle attacker must not leak\n")
			}
			switch when {
			case "after_permission":
				swap()
			case "after_root_open":
				tool.afterRootOpen = swap
			case "after_file_open":
				tool.afterFileOpen = func(string) { swap() }
			}
			result, err := tool.Execute(ctx, in)
			if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "attacker must not leak") {
				t.Fatalf("swapped file result = %+v, %v; want fail-closed", result, err)
			}
		})
	}
}

func TestGrepSingleFilePreservesPermissionAndInvocationBinding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.go")
	mustWritePinnedTestFile(t, path, "needle private\n")
	gate := permission.New(permission.ModeBypassPermissions)
	gate.AppendRules(permission.Rule{Tool: "Grep", Match: path, Verb: permission.DecisionDeny, Source: "test:denied-file"})
	tool := NewGrep(gate)
	in := map[string]any{"root": path, "pattern": "needle"}
	if got, reason := tool.CanUse(context.Background(), in); got != tools.PermissionDeny || reason != "test:denied-file" {
		t.Fatalf("file rule: CanUse = %v (%s), want rule denial", got, reason)
	}
	result, err := tool.Execute(context.Background(), in)
	if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "needle private") {
		t.Fatalf("denied file result = %+v, %v", result, err)
	}

	tool = NewGrep(permission.New(permission.ModeBypassPermissions))
	ctx := tools.WithInvocationID(context.Background(), "single-file-approved")
	if got, reason := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v (%s), want allow", got, reason)
	}
	otherCtx := tools.WithInvocationID(context.Background(), "single-file-unapproved")
	result, err = tool.Execute(otherCtx, in)
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "binding missing") {
		t.Fatalf("unbound file result = %+v, %v", result, err)
	}
	in["pattern"] = "private"
	result, err = tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "input changed") {
		t.Fatalf("changed input result = %+v, %v", result, err)
	}
}

func TestGrepSingleFileRejectsSymlinkRetargetAfterOpen(t *testing.T) {
	root := t.TempDir()
	safe := filepath.Join(root, "safe.go")
	attacker := filepath.Join(root, "attacker.go")
	link := filepath.Join(root, "target.go")
	mustWritePinnedTestFile(t, safe, "needle approved\n")
	mustWritePinnedTestFile(t, attacker, "needle attacker must not leak\n")
	if err := os.Symlink(safe, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	tool := NewGrep(permission.New(permission.ModeBypassPermissions))
	ctx := tools.WithInvocationID(context.Background(), "single-file-symlink")
	in := map[string]any{"root": link, "pattern": "needle"}
	if got, reason := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v (%s), want allow", got, reason)
	}
	tool.afterFileOpen = func(string) {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, link); err != nil {
			t.Fatal(err)
		}
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "attacker must not leak") {
		t.Fatalf("retargeted symlink result = %+v, %v; want fail-closed", result, err)
	}
}

func TestGrepSingleFileKeepsCredentialGuards(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	mustWritePinnedTestFile(t, path, "CUSTOM_API_KEY=plain-looking-secret\nordinary=kept\n")
	result, err := NewGrep(nil).Execute(context.Background(), map[string]any{"root": path, "pattern": "="})
	if err != nil || result == nil || result.IsError || strings.Contains(result.Output, "plain-looking-secret") ||
		!strings.Contains(result.Output, "CUSTOM_API_KEY=[REDACTED]") || !strings.Contains(result.Output, "ordinary=kept") {
		t.Fatalf("single-file redaction = %+v, %v", result, err)
	}
	secret := filepath.Join(root, ".env")
	mustWritePinnedTestFile(t, secret, "PASSWORD=must-not-leak\n")
	tool := NewGrep(permission.New(permission.ModeBypassPermissions))
	in := map[string]any{"root": secret, "pattern": "."}
	if got, _ := tool.CanUse(context.Background(), in); got != tools.PermissionDeny {
		t.Fatalf("secret-file CanUse = %v, want deny", got)
	}
	result, err = tool.Execute(context.Background(), in)
	if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "must-not-leak") {
		t.Fatalf("secret-file result = %+v, %v", result, err)
	}
}

func TestGrepSingleFileSkipsResolvedCredentialPathWithoutGate(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, ".env")
	link := filepath.Join(root, "notes.txt")
	mustWritePinnedTestFile(t, secret, "private content must not appear\n")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	result, err := NewGrep(nil).Execute(context.Background(), map[string]any{"root": link, "pattern": "."})
	if err != nil || result == nil || strings.Contains(result.Output, "private content must not appear") ||
		!strings.Contains(result.Output, "credential file(s) skipped") {
		t.Fatalf("resolved credential file = %+v, %v; want skipped", result, err)
	}
}
