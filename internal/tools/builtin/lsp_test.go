package builtin

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

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
	for _, lang := range []string{"python", "typescript", "javascript", "rust"} {
		if _, ok := stdioLSPServerFor(lang); !ok {
			t.Errorf("expected a server config for %q", lang)
		}
	}
	if _, ok := stdioLSPServerFor("go"); ok {
		t.Error("go must NOT be in the stdio table (driven via gopls CLI)")
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
