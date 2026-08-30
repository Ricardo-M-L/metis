package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// LSP is a "minimum useful" LSP query tool. Every language is driven through
// a minimal stdio LSP client (lsp_client.go) that spins a fresh server per
// query — gopls (Go), pyright (Python), typescript-language-server (TS/JS),
// rust-analyzer (Rust). Languages with no installed backend degrade gracefully
// with a clear message rather than pretending.
//
// Action set:
//   - hover       — return hover text at file:line:col
//   - definition  — find where the symbol at file:line:col is defined
//   - references  — find every reference to the symbol at file:line:col
//   - implementations — find implementations at file:line:col
//
// LSP intentionally does NOT embed tools.BaseTool — it implements its
// own IsEnabled() that checks whether ANY supported backend is on PATH.
// When none is present the tool is hidden from the model entirely so it
// never gets a tool call it can only fail. This is the "tool
// self-decides based on environment" pattern claude-code uses for tools
// like WebBrowser (depends on chromium binary).
type LSP struct {
	gate       *permission.Gate
	manager    *sandbox.Manager
	authorizer *invocationAuthorizer[lspInvocationBinding]
	// afterOpen is a deterministic test seam. Production constructors leave it
	// nil; approved source bytes are always read from the already pinned handle.
	afterOpen func()
}

type lspInvocationKey struct {
	action string
	path   string
	line   int
	column int
}

type lspInvocationBinding struct {
	input  lspInvocationKey
	source approvedExistingPath
}

func lspInvocationFromInput(in map[string]any) lspInvocationKey {
	return lspInvocationKey{
		action: strFromAny(in["action"]),
		path:   strFromAny(in["path"]),
		line:   intArg(in, "line", 0),
		column: intArg(in, "column", 0),
	}
}

// NewLSP constructs the legacy unsandboxed LSP tool. Runtime construction
// should use NewLSPWithSandbox so every language server shares the same
// Manager as Bash, Git, RunCode, and Workflow.
func NewLSP(gate *permission.Gate) LSP {
	return LSP{gate: gate, authorizer: newInvocationAuthorizer[lspInvocationBinding]()}
}

// NewLSPWithSandbox constructs LSP with the runtime-owned sandbox Manager.
func NewLSPWithSandbox(gate *permission.Gate, manager *sandbox.Manager) LSP {
	return LSP{gate: gate, manager: manager, authorizer: newInvocationAuthorizer[lspInvocationBinding]()}
}

// WithSandbox returns a copy wired to manager. LSP remains a value tool for
// compatibility with the historical registry and direct test construction.
func (l LSP) WithSandbox(manager *sandbox.Manager) LSP {
	l.manager = manager
	return l
}

// SandboxManager returns the Manager applied to gopls and stdio servers.
func (l LSP) SandboxManager() *sandbox.Manager { return l.manager }

// IsEnabled reports whether the LSP tool can function here — i.e. at
// least one supported backend (gopls / pyright / typescript-language-
// server / rust-analyzer) is on PATH. Called once at registration; the
// exec.LookPath cost (a few stat calls) is negligible.
//
// Rationale for self-disable rather than degrade-and-error: a tool that
// 100% errors on every input is noise in the model's tool palette.
// Hiding it (IsEnabled=false → reg.Restrict filters out) is cleaner —
// model doesn't see it, model doesn't try it. Mirrors claude-code's
// Tool.isEnabled (Tool.ts:403).
func (LSP) IsEnabled() bool {
	return anyLSPServerAvailable()
}

func (LSP) Name() string { return "LSP" }
func (LSP) Description() string {
	return "Query LSP-style semantics (hover / definition / references / implementations) at file:line:col. Backed by gopls (Go), pyright (Python), typescript-language-server (TS/JS), and rust-analyzer (Rust) when installed; languages with no installed server return a friendly fallback."
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

func (l LSP) prepareAuthorizedBinding(in map[string]any) (lspInvocationBinding, error) {
	path := strFromAny(in["path"])
	source, err := prepareExistingPath(path, false)
	if err != nil {
		return lspInvocationBinding{}, err
	}
	if !source.matchesCurrent(source.targetInfo) {
		return lspInvocationBinding{}, errors.New("LSP source changed during permission preparation")
	}
	return lspInvocationBinding{input: lspInvocationFromInput(in), source: source}, nil
}

func (l LSP) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	binding, err := l.prepareAuthorizedBinding(in)
	if err != nil {
		return err
	}
	l.authorizer.record(ctx, binding)
	return nil
}

func (l LSP) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := strFromAny(in["path"])
	if l.gate == nil {
		return tools.PermissionAsk, "LSP requires a permission gate"
	}
	// Prepare both the lexical and resolved inode before recording permission.
	// The exact dispatcher invocation ID is the only key that can consume this
	// binding; identical later input cannot inherit a denied/cancelled ASK.
	binding, inspectErr := l.prepareAuthorizedBinding(in)
	d, src := l.gate.CheckPath(ctx, "LSP", path, path)
	if inspectErr == nil {
		if secretDecision, secretSource, protected := lspCredentialDecision(l.gate, path, binding.source.resolvedPath); protected &&
			(d != permission.DecisionDeny || src == "mode:plan" || src == "mode:dontAsk") {
			d, src = secretDecision, secretSource
		}
		if d != permission.DecisionDeny && l.authorizer != nil && in != nil {
			l.authorizer.record(ctx, binding)
		}
	} else if d != permission.DecisionDeny {
		return tools.PermissionDeny, security.RedactSubprocessText(inspectErr.Error())
	}
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
	// Drive every language, including Go, through didOpen with bytes read from
	// the approved descriptor. The old gopls CLI path reopened the pathname after
	// permission and could observe a swapped symlink/inode.
	srv, known := stdioLSPServerFor(lang)
	if !known {
		return &tools.Result{
			Output: fmt.Sprintf("LSP backend for %s not configured (supported: Go, Python, TypeScript, JavaScript, Rust). (file: %s)", lang, filepath.Base(path)),
		}, nil
	}
	if !srv.available() {
		return &tools.Result{
			Output: fmt.Sprintf("LSP server %q for %s is not installed — `%s` not on PATH. (file: %s)", srv.cmd, lang, srv.cmd, filepath.Base(path)),
		}, nil
	}
	binding, boundaryResult := l.authorizedSource(ctx, in, path)
	if boundaryResult != nil {
		return boundaryResult, nil
	}
	return runApprovedStdioLSPQueryWithSandbox(ctx, srv, action, binding.source, line, col, l.manager, l.afterOpen)
}

// authorizedSource returns the exact lexical/resolved inode prepared by CanUse.
// Direct embedders without an invocation ID are supported only after a fresh
// fail-closed permission check.
func (l LSP) authorizedSource(ctx context.Context, in map[string]any, path string) (lspInvocationBinding, *tools.Result) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared, hasInvocationID, found := l.authorizer.consume(ctx)
	if hasInvocationID {
		if !found {
			return lspInvocationBinding{}, &tools.Result{Output: "LSP denied: permission binding missing for this invocation", IsError: true}
		}
		if prepared.input != lspInvocationFromInput(in) {
			return lspInvocationBinding{}, &tools.Result{Output: "LSP denied: invocation input changed after permission check", IsError: true}
		}
		return prepared, nil
	}
	if l.gate == nil {
		return lspInvocationBinding{}, &tools.Result{Output: "LSP denied: permission gate unavailable", IsError: true}
	}
	preparedSource, err := prepareExistingPath(path, false)
	if err != nil {
		return lspInvocationBinding{}, &tools.Result{
			Output:  fmt.Sprintf("LSP: cannot inspect %s: %s", filepath.Base(path), security.RedactSubprocessText(err.Error())),
			IsError: true,
		}
	}
	decision, gateSource := l.gate.CheckPath(ctx, "LSP", path, path)
	if decision != permission.DecisionAllow {
		return lspInvocationBinding{}, lspBoundaryDenied(gateSource)
	}
	if secretDecision, secretSource, protected := lspCredentialDecision(l.gate, path, preparedSource.resolvedPath); protected && secretDecision != permission.DecisionAllow {
		return lspInvocationBinding{}, lspBoundaryDenied(secretSource)
	}

	// Re-check the canonical target after resolution. This protects a direct
	// Execute caller from a symlink target that changed between the lexical
	// CheckPath and EvalSymlinks calls above.
	decision, gateSource = l.gate.CheckPath(ctx, "LSP", preparedSource.resolvedPath, preparedSource.resolvedPath)
	if decision != permission.DecisionAllow {
		return lspInvocationBinding{}, lspBoundaryDenied(gateSource)
	}
	return lspInvocationBinding{input: lspInvocationFromInput(in), source: preparedSource}, nil
}

func lspBoundaryDenied(source string) *tools.Result {
	source = strings.TrimSpace(security.RedactSubprocessText(source))
	if strings.Contains(source, "secret_read") {
		return &tools.Result{Output: "LSP denied: credential files are unavailable without explicit interactive approval", IsError: true}
	}
	if source == "" {
		source = "permission policy"
	}
	return &tools.Result{Output: "LSP denied: " + source, IsError: true}
}

// LSP exposes source contents through didOpen, so it needs the same
// bypass-immune credential rule as Read even though permission's historical
// readPathTools table predates LSP. Interactive modes may explicitly approve;
// unattended postures fail closed and never surface an approval dialog.
func lspCredentialDecision(gate *permission.Gate, lexical, resolved string) (permission.Decision, string, bool) {
	if !permission.IsSecretReadPath(lexical) && !permission.IsSecretReadPath(resolved) {
		return permission.DecisionAllow, "", false
	}
	if gate == nil {
		return permission.DecisionDeny, "secret_read:bypass_immune", true
	}
	switch gate.Mode() {
	case permission.ModeBypassPermissions, permission.ModeDontAsk:
		return permission.DecisionDeny, "secret_read:bypass_immune", true
	default:
		return permission.DecisionAsk, "secret_read:bypass_immune", true
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
	return runGoplsQueryWithSandbox(ctx, action, path, line, col, nil)
}

func runGoplsQueryWithSandbox(ctx context.Context, action, path string, line, col int, manager *sandbox.Manager) (*tools.Result, error) {
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
	// gopls is a model-triggered subprocess just like the stdio LSP servers.
	// Do not expose provider keys, connector tokens, or authentication-agent
	// sockets from the Metis process environment.
	root := lspProjectRoot(path, []string{"go.work", "go.mod", ".git"})
	cmd.Dir = root
	// A non-nil Cmd.Env disables os/exec's automatic PWD rewrite for Cmd.Dir.
	// Keep PWD aligned so gopls and compiler helpers resolve the same project.
	cmd.Env = security.RestrictedSubprocessEnv(os.Environ(), "PWD="+root)
	if manager != nil {
		// FilterEnv is deliberately applied to the already allow-listed
		// environment: it normalizes TMPDIR to the Manager's private writable
		// directory without broadening RestrictedSubprocessEnv.
		cmd.Env = manager.FilterEnv(cmd.Env, false)
		wrapped, err := manager.Wrap(cmd, sandbox.Request{Cwd: root})
		if err != nil {
			return &tools.Result{
				Output:  "LSP: sandbox wrap failed for gopls: " + security.RedactSubprocessText(err.Error()),
				IsError: true,
			}, nil
		}
		cmd = wrapped
	}
	configureLSPProcess(cmd)
	out, err := runLSPCombinedOutput(cmd)
	body := security.RedactSubprocessText(strings.TrimSpace(string(out)))
	if err != nil {
		// gopls writes useful diagnostics to stderr (which CombinedOutput
		// merges); surface them so the user sees compile errors etc.
		return &tools.Result{Output: body, IsError: true}, nil
	}
	if body == "" {
		body = "(no result)"
	}
	return &tools.Result{Output: body}, nil
}
