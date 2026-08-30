package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestLSPConstructorsRetainSandboxManager(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeBypass)

	if got := NewLSPWithSandbox(gate, manager).SandboxManager(); got != manager {
		t.Fatal("NewLSPWithSandbox did not retain Manager")
	}
	if got := NewLSP(gate).WithSandbox(manager).SandboxManager(); got != manager {
		t.Fatal("LSP.WithSandbox did not retain Manager")
	}
	if got := NewLSP(gate).SandboxManager(); got != nil {
		t.Fatalf("legacy NewLSP Manager = %p, want nil", got)
	}
}

func TestLSPExecuteFailsClosedOnBypassCredentialSymlinkBeforeServerSpawn(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	secretDir := filepath.Join(root, ".ssh")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(secretDir, "credential.py")
	if err := os.WriteFile(secret, []byte("api_key = 'must-not-read'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "innocent.py")
	if err := os.Symlink(secret, alias); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "server-ran")
	serverDir := t.TempDir()
	server := filepath.Join(serverDir, "pyright-langserver")
	if err := os.WriteFile(server, []byte("#!/bin/sh\nprintf ran > "+sentinel+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", serverDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewLSP(permission.New(permission.ModeBypass))
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "hover", "path": alias, "line": 1, "column": 1,
	})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "credential") {
		t.Fatalf("credential-path result = %#v", res)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("language server ran before credential denial: stat error = %v", err)
	}
}

func TestLSPCanUseAppliesCredentialBoundaryToResolvedTarget(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	secretDir := filepath.Join(root, ".ssh")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(secretDir, "credential.py")
	if err := os.WriteFile(secret, []byte("secret = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "ordinary.py")
	if err := os.Symlink(secret, alias); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"action": "hover", "path": alias, "line": 1, "column": 1}

	for _, tc := range []struct {
		mode permission.Mode
		want tools.Permission
	}{
		{mode: permission.ModeDefault, want: tools.PermissionAsk},
		{mode: permission.ModeAcceptEdits, want: tools.PermissionAsk},
		{mode: permission.ModePlan, want: tools.PermissionAsk},
		{mode: permission.ModeDontAsk, want: tools.PermissionDeny},
		{mode: permission.ModeBypassPermissions, want: tools.PermissionDeny},
	} {
		tool := NewLSP(permission.New(tc.mode))
		got, source := tool.CanUse(context.Background(), in)
		if got != tc.want || source != "secret_read:bypass_immune" {
			t.Errorf("mode %s credential symlink = %v (%s), want %v (secret_read:bypass_immune)", tc.mode, got, source, tc.want)
		}
	}
}

func TestLSPCanUseAppliesScopeBoundaryToResolvedTarget(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "outside.py")
	if err := os.WriteFile(target, []byte("value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "inside.py")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	inScope := func(path string) bool {
		abs, err := filepath.Abs(path)
		if err != nil {
			return false
		}
		rel, err := filepath.Rel(workspace, abs)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}

	for _, tc := range []struct {
		mode   permission.Mode
		want   tools.Permission
		source string
	}{
		{mode: permission.ModeDefault, want: tools.PermissionAsk, source: "scope:outside"},
		{mode: permission.ModeDontAsk, want: tools.PermissionDeny, source: "mode:dontAsk:scope"},
		{mode: permission.ModeBypassPermissions, want: tools.PermissionAllow, source: "mode:bypassPermissions"},
	} {
		gate := permission.New(tc.mode)
		gate.SetPathScopeHook(inScope)
		tool := NewLSP(gate)
		in := map[string]any{"action": "hover", "path": alias, "line": 1, "column": 1}
		got, source := tool.CanUse(context.Background(), in)
		if got != tc.want || source != tc.source {
			t.Errorf("mode %s scope symlink = %v (%s), want %v (%s)", tc.mode, got, source, tc.want, tc.source)
		}
	}
}

func TestLSPExecuteRejectsPathChangedAfterCanUse(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	first := filepath.Join(root, "first.py")
	second := filepath.Join(root, "second.py")
	if err := os.WriteFile(first, []byte("first = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "current.py")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "server-ran")
	serverDir := t.TempDir()
	server := filepath.Join(serverDir, "pyright-langserver")
	if err := os.WriteFile(server, []byte("#!/bin/sh\nprintf ran > "+sentinel+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", serverDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewLSP(permission.New(permission.ModeBypass))
	in := map[string]any{"action": "hover", "path": alias, "line": 1, "column": 1}
	ctx := tools.WithInvocationID(context.Background(), "lsp-path-swap")
	if got, source := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v (%s), want allow", got, source)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}

	res, err := tool.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "changed") {
		t.Fatalf("swapped-symlink result = %#v", res)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("language server ran after symlink swap: stat error = %v", err)
	}
}

func TestLSPDeniedBindingCannotAuthorizeLaterInvocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.py")
	if err := os.WriteFile(path, []byte("value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverDir := t.TempDir()
	server := filepath.Join(serverDir, "pyright-langserver")
	if err := os.WriteFile(server, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", serverDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	gate := permission.New(permission.ModeDefault)
	gate.AppendRules(permission.Rule{Tool: "LSP", Match: path, Verb: permission.DecisionAsk, Source: "test:ask"})
	tool := NewLSP(gate)
	in := map[string]any{"action": "hover", "path": path, "line": 1, "column": 1}
	deniedCtx := tools.WithInvocationID(context.Background(), "lsp-denied")
	if got, _ := tool.CanUse(deniedCtx, in); got != tools.PermissionAsk {
		t.Fatalf("CanUse = %v, want ASK", got)
	}

	laterCtx := tools.WithInvocationID(context.Background(), "lsp-later")
	res, err := tool.Execute(laterCtx, in)
	if err != nil || res == nil || !res.IsError || !strings.Contains(res.Output, "binding missing") {
		t.Fatalf("later Execute = %#v, %v; want exact-invocation refusal", res, err)
	}
}

func TestLSPReadsPinnedSourceAfterAuthorizedPathReplacement(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("atomic replacement of an open file is platform-specific")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.py")
	replacement := filepath.Join(dir, "replacement.py")
	if err := os.WriteFile(source, []byte("approved_source = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("replacement_source = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	serverDir := t.TempDir()
	server := filepath.Join(serverDir, "pyright-langserver")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestLSPHelperProcess$ -- lsp-helper-process lsp-helper-echo-open\n", executable)
	if err := os.WriteFile(server, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", serverDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewLSP(permission.New(permission.ModeBypassPermissions))
	tool.afterOpen = func() {
		if err := os.Rename(replacement, source); err != nil {
			t.Fatalf("replace approved path: %v", err)
		}
	}
	ctx := tools.WithInvocationID(context.Background(), "lsp-pinned-source")
	in := map[string]any{"action": "hover", "path": source, "line": 1, "column": 1}
	if got, reason := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v (%s), want allow", got, reason)
	}
	res, err := tool.Execute(ctx, in)
	if err != nil || res == nil || res.IsError || !strings.Contains(res.Output, "approved_source") || strings.Contains(res.Output, "replacement_source") {
		t.Fatalf("pinned LSP result = %#v, %v", res, err)
	}
}

func TestLSP_RejectsRelativePath(t *testing.T) {
	tool := LSP{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": "rel.go", "line": 1, "column": 1,
	})
	if err == nil {
		t.Errorf("relative path should error")
	}
}

func TestLSP_RejectsZeroLineCol(t *testing.T) {
	tool := LSP{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": "/tmp/x.go", "line": 0, "column": 1,
	})
	if err == nil {
		t.Errorf("line=0 should error")
	}
	_, err = tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": "/tmp/x.go", "line": 1, "column": 0,
	})
	if err == nil {
		t.Errorf("column=0 should error")
	}
}

func TestLSP_RequiresAction(t *testing.T) {
	tool := LSP{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": "/tmp/x.go", "line": 1, "column": 1,
	})
	if err == nil {
		t.Errorf("missing action should error")
	}
}

func TestLSP_NonGoLanguageDegradeGracefully(t *testing.T) {
	// When the language's server isn't installed, Execute must degrade
	// to a friendly non-error message rather than failing. If pyright
	// happens to be installed in this environment we skip — it would
	// (correctly) error on the nonexistent /tmp/x.py file.
	if srv, ok := stdioLSPServerFor("python"); ok && srv.available() {
		t.Skip("pyright installed; skipping the no-backend degrade assertion")
	}
	tool := LSP{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": "/tmp/x.py", "line": 1, "column": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("non-Go with no backend should NOT mark as error, just an info message")
	}
}

func TestLSP_UnknownLanguageDegradeGracefully(t *testing.T) {
	// A language metis has no server table entry for must always degrade
	// to a non-error info message, regardless of installed tooling.
	tool := LSP{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": "/tmp/x.rb", "line": 1, "column": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("unknown language should degrade to a non-error info message")
	}
}

func TestStdioLSPServerFor(t *testing.T) {
	for _, lang := range []string{"go", "python", "typescript", "javascript", "rust"} {
		if _, ok := stdioLSPServerFor(lang); !ok {
			t.Errorf("expected a server config for %q", lang)
		}
	}
	if _, ok := stdioLSPServerFor("ruby"); ok {
		t.Error("ruby has no configured server; expected ok=false")
	}
}

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"x.go":  "go",
		"x.py":  "python",
		"x.ts":  "typescript",
		"x.tsx": "typescript",
		"x.js":  "javascript",
		"x.rs":  "rust",
		"x.foo": "unknown",
	}
	for in, want := range cases {
		if got := detectLanguage(in); got != want {
			t.Errorf("detectLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}
