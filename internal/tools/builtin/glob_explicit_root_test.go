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

func TestGlobSearchesSelectedHeavyRoot(t *testing.T) {
	for _, name := range []string{"build", "dist", "vendor", "target"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			root := filepath.Join(workspace, name)
			writeGlobFile(t, root, "nested/hit.go")
			for _, tc := range []struct {
				name string
				cwd  string
				root string
				want string
			}{
				{"absolute root", workspace, root, filepath.Join(root, "nested/hit.go")},
				{"relative root", workspace, name, filepath.Join(name, "nested/hit.go")},
				{"current workspace", root, "", filepath.FromSlash("nested/hit.go")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := agent.WithCwd(context.Background(), tc.cwd)
					res, err := (Glob{}).Execute(ctx, map[string]any{"root": tc.root, "pattern": "**/*.go"})
					if err != nil || res == nil || res.IsError {
						t.Fatalf("Execute: err=%v result=%+v", err, res)
					}
					assertGlobPaths(t, res.Output, []string{tc.want})
				})
			}
		})
	}
}

func TestGlobSearchesLiteralHeavyPatternPrefix(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"build/hit.go", "build/nested/hit.go", "build/vendor/hit.go", "dist/hit.go", "src/hit.go"} {
		writeGlobFile(t, root, file)
	}
	for _, pattern := range []string{"build/**/*.go", "./build/**/*.go", filepath.Join(root, "build", "**", "*.go")} {
		t.Run(pattern, func(t *testing.T) {
			res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": pattern})
			if err != nil || res == nil || res.IsError {
				t.Fatalf("Execute: err=%v result=%+v", err, res)
			}
			assertGlobPaths(t, res.Output, []string{filepath.Join(root, "build/hit.go"), filepath.Join(root, "build/nested/hit.go")})
		})
	}
	// A second explicitly named heavy directory is traversable, but the broad
	// recursive wildcard above must not enable all heavy descendants.
	res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "build/vendor/*.go"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("Execute: err=%v result=%+v", err, res)
	}
	assertGlobPaths(t, res.Output, []string{filepath.Join(root, "build/vendor/hit.go")})
}

func TestGlobExplicitPrefixDoesNotExpandSearchBoundary(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeGlobFile(t, root, "build/hit.go")
	writeGlobFile(t, outside, "build/outside.go")
	if err := os.Symlink(outside, filepath.Join(root, "build", "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, pattern := range []string{"build/**/*.go", filepath.Join(outside, "build", "**", "*.go")} {
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": pattern})
		if err != nil || res == nil || res.IsError {
			t.Fatalf("Execute: err=%v result=%+v", err, res)
		}
		if strings.Contains(res.Output, "outside.go") {
			t.Fatalf("Glob traversed outside its root: %q", res.Output)
		}
	}
	// Selecting a symlink as root must not opt into following it either.
	res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": filepath.Join(root, "build", "linked"), "pattern": "**/*.go"})
	if err != nil || res == nil || res.IsError || strings.Contains(res.Output, "outside.go") {
		t.Fatalf("symlink root changed traversal boundary: err=%v result=%+v", err, res)
	}
	gate := permission.New(permission.ModeBypass)
	gate.AppendRules(permission.Rule{Tool: "Glob", Match: filepath.Join(root, "build"), Verb: permission.DecisionDeny, Source: "denied-build"})
	decision, source := NewGlob(gate).CanUse(context.Background(), map[string]any{"root": filepath.Join(root, "build"), "pattern": "**/*.go"})
	if decision != tools.PermissionDeny || source != "denied-build" {
		t.Fatalf("explicit root lost deny rule: %v (%s)", decision, source)
	}
}

func TestGlobReportsIncompleteSearch(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		root := t.TempDir()
		writeGlobFile(t, root, "nested/hit.go")
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "**/*.go", "max_depth": 1})
		if err != nil || res == nil || !strings.Contains(res.Output, "search incomplete") || !strings.Contains(res.Output, "max_depth=1") {
			t.Fatalf("depth-limited empty search needs a recovery hint: err=%v result=%+v", err, res)
		}
	})
	t.Run("limit", func(t *testing.T) {
		root := t.TempDir()
		writeGlobFile(t, root, "a.go")
		writeGlobFile(t, root, "b.go")
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "**/*.go", "limit": 1})
		if err != nil || res == nil || !strings.Contains(res.Output, "search incomplete") || !strings.Contains(res.Output, "showing 1 of 2 matches") {
			t.Fatalf("limited result needs a recovery hint: err=%v result=%+v", err, res)
		}
	})
	t.Run("heavy descendants", func(t *testing.T) {
		root := t.TempDir()
		writeGlobFile(t, root, "build/hit.go")
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "**/*.go"})
		if err != nil || res == nil || !strings.Contains(res.Output, "search incomplete") || !strings.Contains(res.Output, "skipped 1 generated/cache") || strings.Contains(res.Output, "hit.go") {
			t.Fatalf("skipped search needs explicit-root guidance: err=%v result=%+v", err, res)
		}
	})
	t.Run("unreadable directory", func(t *testing.T) {
		root := t.TempDir()
		writeGlobFile(t, root, "unreadable/hit.go")
		unreadable := filepath.Join(root, "unreadable")
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
		if _, err := os.ReadDir(unreadable); err == nil {
			t.Skip("filesystem or privileges permit reads despite directory permissions")
		}
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "**/*.go"})
		if err != nil || res == nil || !strings.Contains(res.Output, "search incomplete") || !strings.Contains(res.Output, "1 unreadable") {
			t.Fatalf("unreadable subtree was reported as an exhaustive miss: err=%v result=%+v", err, res)
		}
	})
}

func TestGlobExplicitPrefixRetainsPermissionDecisions(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, root, "build/hit.go")
	for _, pattern := range []string{"build/**/*.go", filepath.Join(root, "build", "**", "*.go")} {
		for _, verb := range []permission.Decision{permission.DecisionDeny, permission.DecisionAsk} {
			gate := permission.New(permission.ModeDefault)
			gate.SetReadOnlyHook(func(tool, _ string) bool { return tool == "Glob" })
			gate.AppendRules(permission.Rule{Tool: "Glob", Match: filepath.Join(root, "build"), Verb: verb, Source: "prefix-rule"})
			decision, source := NewGlob(gate).CanUse(context.Background(), map[string]any{"root": root, "pattern": pattern})
			if decision != mapDecision(verb) || source != "prefix-rule" {
				t.Errorf("pattern %q rule %v: got %v (%s)", pattern, verb, decision, source)
			}
		}
	}
	gate := permission.New(permission.ModeDefault)
	gate.SetPathScopeHook(func(string) bool { return false })
	gate.AppendRules(permission.Rule{Tool: "Glob", Match: filepath.Join(root, "build"), Verb: permission.DecisionDeny, Source: "prefix-deny"})
	decision, source := NewGlob(gate).CanUse(context.Background(), map[string]any{"root": root, "pattern": "build/**/*.go"})
	if decision != tools.PermissionDeny || source != "prefix-deny" {
		t.Fatalf("root-level approval request masked explicit prefix denial: %v (%s)", decision, source)
	}
}

func TestGlobExplicitPrefixRetainsScopeBoundary(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, root, "build/hit.go")
	gate := permission.New(permission.ModeDefault)
	gate.SetReadOnlyHook(func(tool, _ string) bool { return tool == "Glob" })
	// The runtime supplies its own path-scope policy. An approved parent cannot
	// silently authorize a newly included generated subtree excluded by it.
	gate.SetPathScopeHook(func(path string) bool { return filepath.Base(path) != "build" })
	decision, source := NewGlob(gate).CanUse(context.Background(), map[string]any{"root": root, "pattern": "build/**/*.go"})
	if decision != tools.PermissionAsk || source != "scope:outside" {
		t.Fatalf("explicit prefix escaped the path-scope policy: %v (%s)", decision, source)
	}
	// A generated directory name in an absolute pattern outside the selected
	// root must not trigger filesystem traversal or broaden authorization.
	outside := t.TempDir()
	decision, _ = NewGlob(gate).CanUse(context.Background(), map[string]any{"root": root, "pattern": filepath.Join(outside, "build", "**", "*.go")})
	if decision != tools.PermissionAllow {
		t.Fatalf("non-traversable absolute pattern changed root authorization: %v", decision)
	}
}
