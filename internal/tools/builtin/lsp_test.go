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
	tool := LSP{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": "/tmp/x.py", "line": 1, "column": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("non-Go should NOT mark as error, just an info message")
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
