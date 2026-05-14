package builtin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// LSP is a "minimum useful" LSP query tool that shells out to per-language
// LSP CLIs (gopls / pyright / typescript-language-server stdio probe).
// Not a full LSP client — claude-code's LSPTool is, and metis can grow
// into one later. For now this covers the 80% cases (hover, definition,
// references on Go) and degrades gracefully for other languages.
//
// Action set:
//   - hover       — return hover text at file:line:col
//   - definition  — find where the symbol at file:line:col is defined
//   - references  — find every reference to the symbol at file:line:col
//   - implementations — find implementations (Go interfaces) at file:line:col
//
// Only Go is fully supported (uses `gopls` CLI subcommand). For other
// languages we return a clear "no LSP backend available" rather than
// pretending. Users can install gopls if needed.
//
// LSP intentionally does NOT embed tools.BaseTool — it implements its
// own IsEnabled() that checks whether `gopls` is on PATH. When gopls
// is missing the tool is hidden from the model entirely so it never
// gets a tool call it can only fail. This is the "tool self-decides
// based on environment" pattern claude-code uses for tools like
// WebBrowser (depends on chromium binary).
type LSP struct{ gate *permission.Gate }

// IsEnabled reports whether the LSP tool can actually function in
// this environment. Today that means: gopls is on PATH. Other
// languages would extend this — e.g. add pyright / tsserver checks
// once their handlers land. Called once at registration; the
// exec.LookPath cost (one stat call) is negligible.
//
// Rationale for self-disable rather than degrade-and-error: a tool
// that 100% errors on every input is noise in the model's tool
// palette. Hiding it (IsEnabled=false → reg.Restrict filters out)
// is cleaner — model doesn't see it, model doesn't try it. Mirrors
// claude-code's Tool.isEnabled (Tool.ts:403).
func (LSP) IsEnabled() bool {
	_, err := exec.LookPath("gopls")
	return err == nil
}

func (LSP) Name() string { return "LSP" }
func (LSP) Description() string {
	return "Query LSP-style semantics (hover / definition / references / implementations) at file:line:col. Currently backed by gopls for Go; other languages return a friendly fallback."
}

func (LSP) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action", "path", "line", "column"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"hover", "definition", "references", "implementations"},
				"description": "LSP query type",
			},
			"path":   map[string]any{"type": "string", "description": "absolute file path"},
			"line":   map[string]any{"type": "integer", "description": "1-based line number"},
			"column": map[string]any{"type": "integer", "description": "1-based column number"},
		},
	}
}

func (LSP) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (l LSP) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := l.gate.Check(context.Background(), "LSP", strFromAny(in["path"]))
	return mapDecision(d), src
}

func (l LSP) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	action, _ := in["action"].(string)
	path, _ := in["path"].(string)
	line := intArg(in, "line", 0)
	col := intArg(in, "column", 0)

	if action == "" {
		return nil, errors.New("action is required")
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("path must be an absolute file path")
	}
	if line <= 0 || col <= 0 {
		return nil, errors.New("line and column must both be ≥ 1")
	}

	lang := detectLanguage(path)
	switch lang {
	case "go":
		return runGoplsQuery(ctx, action, path, line, col)
	default:
		return &tools.Result{
			Output: fmt.Sprintf("LSP backend for %s not configured. Install gopls for Go; other languages are TODO. (file: %s)", lang, filepath.Base(path)),
		}, nil
	}
}

func detectLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	default:
		return "unknown"
	}
}

func runGoplsQuery(ctx context.Context, action, path string, line, col int) (*tools.Result, error) {
	if _, err := exec.LookPath("gopls"); err != nil {
		return nil, errors.New("gopls not on PATH (install: `go install golang.org/x/tools/gopls@latest`)")
	}
	loc := path + ":" + strconv.Itoa(line) + ":" + strconv.Itoa(col)
	var cmd *exec.Cmd
	switch action {
	case "hover":
		// gopls has no top-level "hover" cli sub-command; the equivalent
		// is `gopls definition` which prints the surrounding signature
		// + doc comment when available. Acceptable approximation.
		cmd = exec.CommandContext(ctx, "gopls", "definition", "-markdown", loc)
	case "definition":
		cmd = exec.CommandContext(ctx, "gopls", "definition", loc)
	case "references":
		cmd = exec.CommandContext(ctx, "gopls", "references", loc)
	case "implementations":
		cmd = exec.CommandContext(ctx, "gopls", "implementation", loc)
	default:
		return nil, fmt.Errorf("unknown LSP action %q", action)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// gopls writes useful diagnostics to stderr (which CombinedOutput
		// merges); surface them so the user sees compile errors etc.
		return &tools.Result{Output: strings.TrimSpace(string(out)), IsError: true}, nil
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		body = "(no result)"
	}
	return &tools.Result{Output: body}, nil
}
