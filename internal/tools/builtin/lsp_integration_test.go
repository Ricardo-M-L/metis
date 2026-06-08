package builtin

// lsp_integration_test.go — real end-to-end exercise of the stdio LSP
// client against an actual language server. Gated behind METIS_LSP_IT=1
// (and skipped if the server isn't installed) so it never runs in normal
// `go test ./...` — it spawns a subprocess and needs pyright on PATH.
//
// Run locally:  METIS_LSP_IT=1 go test ./internal/tools/builtin/ -run LSP_Integration -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestLSP_Integration_PythonDefinition(t *testing.T) {
	if os.Getenv("METIS_LSP_IT") != "1" {
		t.Skip("set METIS_LSP_IT=1 to run the real LSP server integration test")
	}
	srv, ok := stdioLSPServerFor("python")
	if !ok || !srv.available() {
		t.Skip("pyright-langserver not installed")
	}

	dir := t.TempDir()
	src := "def greet(name):\n    return \"hello \" + name\n\n\ndef main():\n    msg = greet(\"world\")\n    print(msg)\n"
	file := filepath.Join(dir, "sample.py")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := LSP{gate: permission.New(permission.ModeBypass)}

	// definition of the greet() call on line 6, col 11 → def on line 1.
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition", "path": file, "line": 6, "column": 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("definition errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, "sample.py:1:") {
		t.Errorf("expected definition at sample.py:1, got %q", res.Output)
	}

	// hover on the same call should mention the function.
	hov, err := tool.Execute(context.Background(), map[string]any{
		"action": "hover", "path": file, "line": 6, "column": 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hov.IsError || strings.TrimSpace(hov.Output) == "" {
		t.Errorf("hover returned nothing useful: err=%v out=%q", hov.IsError, hov.Output)
	}
	t.Logf("definition: %s", res.Output)
	t.Logf("hover: %s", hov.Output)
}
