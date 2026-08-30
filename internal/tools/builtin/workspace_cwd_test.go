package builtin_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	runtimepkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestWorkspaceCWD_SearchToolsAuthorizeAndExecuteSameRelativeRoot(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(workspaceA)

	writeWorkspaceFile(t, filepath.Join(workspaceA, "from-a.txt"), "needle from A\n")
	writeWorkspaceFile(t, filepath.Join(workspaceB, "from-b.txt"), "needle from B\n")
	writeWorkspaceFile(t, filepath.Join(workspaceA, "src", "nested", "from-a.txt"), "nested A\n")
	writeWorkspaceFile(t, filepath.Join(workspaceB, "src", "nested", "from-b.txt"), "nested B\n")

	allowed := runtimepkg.NewAllowedDirs(nil)
	if err := allowed.RebindCWD(workspaceB); err != nil {
		t.Fatalf("RebindCWD: %v", err)
	}
	gate := permission.New(permission.ModeDefault)
	gate.SetReadOnlyHook(func(tool, _ string) bool {
		switch tool {
		case "Glob", "Grep", "LS":
			return true
		default:
			return false
		}
	})
	var authorizedPath string
	gate.SetPathScopeHook(func(path string) bool {
		authorizedPath = path
		return allowed.Contains(path)
	})
	ctx := agent.WithCwd(context.Background(), workspaceB)

	t.Run("Glob", func(t *testing.T) {
		authorizedPath = ""
		tool := builtin.NewGlob(gate)
		input := map[string]any{"pattern": "src/**/*.txt"}
		got, source := tool.CanUse(ctx, input)
		assertWorkspacePermission(t, got, source)
		assertAuthorizedWorkspace(t, authorizedPath, workspaceB)
		result, err := tool.Execute(ctx, input)
		if err != nil {
			t.Fatalf("Glob.Execute: %v", err)
		}
		assertOnlyWorkspaceB(t, result.Output)
	})

	t.Run("Grep", func(t *testing.T) {
		authorizedPath = ""
		tool := builtin.NewGrep(gate)
		input := map[string]any{"pattern": "needle"}
		got, source := tool.CanUse(ctx, input)
		assertWorkspacePermission(t, got, source)
		assertAuthorizedWorkspace(t, authorizedPath, workspaceB)
		result, err := tool.Execute(ctx, input)
		if err != nil {
			t.Fatalf("Grep.Execute: %v", err)
		}
		if result.IsError {
			t.Fatalf("Grep.Execute returned error result: %s", result.Output)
		}
		assertOnlyWorkspaceB(t, result.Output)
	})

	t.Run("LS", func(t *testing.T) {
		authorizedPath = ""
		tool := builtin.NewLS(gate)
		input := map[string]any{"path": "."}
		got, source := tool.CanUse(ctx, input)
		assertWorkspacePermission(t, got, source)
		assertAuthorizedWorkspace(t, authorizedPath, workspaceB)
		result, err := tool.Execute(ctx, input)
		if err != nil {
			t.Fatalf("LS.Execute: %v", err)
		}
		assertOnlyWorkspaceB(t, result.Output)
	})
}

func TestWorkspaceCWD_GrepApprovalsAreScopedByEffectiveRoot(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(workspaceA, "from-a.txt"), "needle from A\n")
	writeWorkspaceFile(t, filepath.Join(workspaceB, "from-b.txt"), "needle from B\n")

	gate := permission.New(permission.ModeDefault)
	gate.SetReadOnlyHook(func(tool, _ string) bool { return tool == "Grep" })
	gate.SetPathScopeHook(func(string) bool { return true })
	tool := builtin.NewGrep(gate)
	input := map[string]any{"pattern": "needle"}
	ctxA := agent.WithCwd(context.Background(), workspaceA)
	ctxB := agent.WithCwd(context.Background(), workspaceB)

	got, source := tool.CanUse(ctxA, input)
	assertWorkspacePermission(t, got, source)
	got, source = tool.CanUse(ctxB, input)
	assertWorkspacePermission(t, got, source)

	// Consume in reverse order. Identical raw inputs from different sub-agent
	// workspaces must not share one FIFO approval key.
	resultB, err := tool.Execute(ctxB, input)
	if err != nil {
		t.Fatalf("Grep.Execute(B): %v", err)
	}
	if resultB.IsError {
		t.Fatalf("Grep.Execute(B) returned error result: %s", resultB.Output)
	}
	assertOnlyWorkspaceB(t, resultB.Output)

	resultA, err := tool.Execute(ctxA, input)
	if err != nil {
		t.Fatalf("Grep.Execute(A): %v", err)
	}
	if resultA.IsError {
		t.Fatalf("Grep.Execute(A) returned error result: %s", resultA.Output)
	}
	if !strings.Contains(resultA.Output, "from-a.txt") || strings.Contains(resultA.Output, "from-b.txt") {
		t.Fatalf("Grep.Execute(A) output = %q, want only workspace A", resultA.Output)
	}
}

func assertWorkspacePermission(t *testing.T, got tools.Permission, source string) {
	t.Helper()
	if got != tools.PermissionAllow {
		t.Fatalf("permission = %v (%s), want allow", got, source)
	}
}

func assertAuthorizedWorkspace(t *testing.T, got, workspace string) {
	t.Helper()
	got = canonicalTestPath(t, got)
	workspace = canonicalTestPath(t, workspace)
	if filepath.Clean(got) != filepath.Clean(workspace) {
		t.Fatalf("authorized path = %q, want active workspace %q", got, workspace)
	}
}

func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}

func assertOnlyWorkspaceB(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, "from-b.txt") {
		t.Fatalf("output did not read active workspace B: %q", output)
	}
	if strings.Contains(output, "from-a.txt") {
		t.Fatalf("output leaked process workspace A: %q", output)
	}
}

func writeWorkspaceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
