package builtin

// code_mode.go — RunCode tool: execute code in a sandboxed runtime
// without the shell interpreter overhead of Bash. Mirrors harness's
// code-runtime-worker-thread concept: for short code snippets, this
// avoids the shell spawn + interpreter cold-start penalty.
//
// Each call writes the code to a temp file, executes it with the
// appropriate language interpreter, captures stdout/stderr, and
// cleans up. Future iterations could add a persistent worker process
// (IPC-based) for sub-10ms round-trips on short snippets.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// languageConfig describes how to run one language.
type languageConfig struct {
	Extension string   // file extension (including dot)
	Command   string   // interpreter binary
	Args      []string // prefix args (before the file path)
}

var languageMap = map[string]languageConfig{
	"python":     {Extension: ".py", Command: "python3", Args: nil},
	"go":         {Extension: ".go", Command: "go", Args: []string{"run"}},
	"javascript": {Extension: ".js", Command: "node", Args: nil},
	"typescript": {Extension: ".ts", Command: "npx", Args: []string{"tsx"}},
	"bash":       {Extension: ".sh", Command: "bash", Args: nil},
	"ruby":       {Extension: ".rb", Command: "ruby", Args: nil},
	"r":          {Extension: ".R", Command: "Rscript", Args: nil},
	"perl":       {Extension: ".pl", Command: "perl", Args: nil},
	"php":        {Extension: ".php", Command: "php", Args: nil},
}

// RunCode executes a code snippet in a sandboxed runtime.
type RunCode struct {
	tools.BaseTool
	gate    *permission.Gate
	manager *sandbox.Manager
}

func NewRunCode(gate *permission.Gate) RunCode {
	return RunCode{gate: gate}
}

func NewRunCodeWithSandbox(gate *permission.Gate, m *sandbox.Manager) RunCode {
	return RunCode{gate: gate, manager: m}
}

func (RunCode) Name() string { return "RunCode" }

func (RunCode) Description() string {
	return `Execute code in a sandboxed runtime. Preferred over Bash for running code snippets — it avoids shell interpreter overhead and quoting issues.

Supported languages: python, go, javascript, typescript, bash, ruby, r, perl, php.

For long-running code, set a reasonable timeout. For very short snippets (print/return), this is significantly faster than spawning a shell.

Do NOT use this for:
- Installing packages (use Bash for pip install, npm install, go get)
- Multi-step build processes (use Bash or Workflow)
- Compiled languages that need a build step separate from running (e.g. rust, c/c++) — use Bash
- Interactive programs (use Bash)
- Operations that need shell features (pipes, redirects, env vars) — use Bash`
}

func (RunCode) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"code", "language"},
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "The code to execute. Must be a complete, runnable program or script.",
			},
			"language": map[string]any{
				"type":        "string",
				"description": "Programming language. Supported: python, go, javascript, typescript, bash, ruby, r, perl, php.",
				"enum":        []string{"python", "go", "javascript", "typescript", "bash", "ruby", "r", "perl", "php"},
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Timeout in seconds (default 30, max 300).",
			},
		},
	}
}

func (RunCode) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (r RunCode) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	code, _ := in["code"].(string)
	d, src := r.gate.Check(context.Background(), "RunCode", truncate(code, 80))
	return mapDecision(d), src
}

func (r RunCode) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	code, _ := in["code"].(string)
	code = strings.TrimSpace(code)
	if code == "" {
		return errResult("RunCode: code is required"), nil
	}
	lang, _ := in["language"].(string)
	lang = strings.ToLower(strings.TrimSpace(lang))
	cfg, ok := languageMap[lang]
	if !ok {
		return errResult(fmt.Sprintf("RunCode: unsupported language %q", lang)), nil
	}

	timeoutSec := 30
	if t, ok := in["timeout"].(float64); ok && t > 0 {
		timeoutSec = int(t)
		if timeoutSec > 300 {
			timeoutSec = 300
		}
	}

	// Write code to temp file
	tmpDir, err := os.MkdirTemp("", "metis-runcode-")
	if err != nil {
		return nil, fmt.Errorf("RunCode: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	srcPath := filepath.Join(tmpDir, "main"+cfg.Extension)
	if err := os.WriteFile(srcPath, []byte(code), 0o644); err != nil {
		return nil, fmt.Errorf("RunCode: write temp file: %w", err)
	}

	// Build the command
	var args []string
	args = append(args, cfg.Args...)
	args = append(args, srcPath)

	cmd := exec.Command(cfg.Command, args...)
	cmd.Dir = tmpDir
	// Own process group so a timeout kills the WHOLE tree: `go run` and
	// `npx tsx` spawn real interpreters that outlive the wrapper kill.
	jobs.ApplyProcessGroup(cmd)

	// Apply sandbox if available
	if r.manager != nil {
		wrapped, err := r.manager.Wrap(cmd, sandbox.Request{
			Cwd:     tmpDir,
			Network: sandbox.NetworkBlock,
		})
		if err != nil {
			return nil, fmt.Errorf("RunCode: sandbox wrap: %w", err)
		}
		cmd = wrapped
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run with timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("RunCode: start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return &tools.Result{
					Output:  fmt.Sprintf("exit code %d\nstderr:\n%s\nstdout:\n%s", exitErr.ExitCode(), strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String())),
					IsError: true,
				}, nil
			}
			return nil, fmt.Errorf("RunCode: execute: %w", err)
		}
	case <-ctx.Done():
		jobs.KillProcessGroup(cmd.Process)
		<-done
		return &tools.Result{
			Output:  fmt.Sprintf("timed out after %ds\nstdout:\n%s\nstderr:\n%s", timeoutSec, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())),
			IsError: true,
		}, nil
	}

	out := strings.TrimSpace(stdout.String())
	if stderr.Len() > 0 {
		errOut := strings.TrimSpace(stderr.String())
		if out != "" {
			out = fmt.Sprintf("stdout:\n%s\n\nstderr:\n%s", out, errOut)
		} else {
			out = fmt.Sprintf("stderr:\n%s", errOut)
		}
	}

	return &tools.Result{Output: out}, nil
}
