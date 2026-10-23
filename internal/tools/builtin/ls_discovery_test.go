package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestLSMissingDirectorySuggestsConfirmedParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "native", "system")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "private-listing.txt"), []byte("not requested"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"src", filepath.Join("wrong", "deep", "src")} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(parent, suffix)
			res, err := NewLS(bypassGate()).Execute(context.Background(), map[string]any{"path": path})
			if err != nil || res == nil || !res.IsError {
				t.Fatalf("missing directory should return an actionable error result: result=%v err=%v", res, err)
			}
			for _, want := range []string{"Directory not found:", path, "Nearest existing directory:", parent, "LS", "Glob"} {
				if !strings.Contains(res.Output, want) {
					t.Errorf("missing %q in %q", want, res.Output)
				}
			}
			if strings.Contains(res.Output, "private-listing.txt") {
				t.Fatal("recovery hint must not list another directory implicitly")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("requested directory must remain absent: %v", err)
			}
		})
	}
}

func TestLSMissingDirectoryRespectsParentScope(t *testing.T) {
	root := t.TempDir()
	gate := permission.New(permission.ModeDefault)
	gate.SetReadOnlyHook(func(tool, _ string) bool { return tool == "LS" })
	gate.SetPathScopeHook(func(string) bool { return false })
	res, err := NewLS(gate).Execute(context.Background(), map[string]any{"path": filepath.Join(root, "missing")})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("result=%v err=%v", res, err)
	}
	if strings.Contains(res.Output, "Nearest existing directory:") {
		t.Fatalf("an unapproved ancestor must not be probed or suggested as confirmed: %q", res.Output)
	}
}

func TestLSMissingRelativeDirectoryUsesInvocationCWD(t *testing.T) {
	root := t.TempDir()
	res, err := NewLS(bypassGate()).Execute(agent.WithCwd(context.Background(), root), map[string]any{"path": "missing"})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("result=%v err=%v", res, err)
	}
	if !strings.Contains(res.Output, filepath.Join(root, "missing")) || !strings.Contains(res.Output, "Nearest existing directory: "+root) {
		t.Fatalf("hint must use the invocation's working directory: %q", res.Output)
	}
}

func TestLSMissingDirectoryCancelledDuringParentPermissionCheck(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := permission.New(permission.ModeDefault)
	gate.SetReadOnlyHook(func(tool, _ string) bool { return tool == "LS" })
	gate.SetPathScopeHook(func(string) bool {
		cancel()
		return true
	})
	res, err := NewLS(gate).Execute(ctx, map[string]any{"path": filepath.Join(root, "missing")})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("result=%v err=%v", res, err)
	}
	if strings.Contains(res.Output, "Nearest existing directory:") {
		t.Fatalf("cancelled discovery must stop before probing a parent: %q", res.Output)
	}
}
