package builtin

// code_mode.go — RunCode tool: execute code under the configured sandbox
// policy without the shell interpreter overhead of Bash. Mirrors harness's
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
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
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

// RunCode executes a code snippet using the configured sandbox policy.
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
	return `Execute a code snippet using the configured sandbox policy. When the OS sandbox is disabled, the process still receives a credential-filtered environment but is not filesystem-sandboxed. Preferred over Bash for running code snippets — it avoids shell interpreter overhead and quoting issues.

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
	// Arbitrary user/model supplied code can mutate the workspace, access the
	// network allowed by the active sandbox policy, and spawn child processes.
	// It must therefore be serialized with other mutating tools.
	return tools.ConcurrencyExclusive
}

func (r RunCode) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	code, _ := in["code"].(string)
	if r.gate == nil {
		return tools.PermissionAsk, "RunCode requires a permission gate"
	}
	// Pass the complete snippet: a credential path after an import/header must
	// not evade the secret-read boundary merely because it appears after byte 80.
	d, src := r.gate.Check(ctx, "RunCode", code)
	return mapDecision(d), src
}

func (r RunCode) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
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
	tmpRoot := ""
	if r.manager != nil {
		tmpRoot = r.manager.TempDir()
		if tmpRoot == "" {
			return nil, fmt.Errorf("RunCode: create temp dir: %w", sandbox.ErrManagerClosed)
		}
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "metis-runcode-")
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
	cmd.Env = security.RestrictedSubprocessEnv(os.Environ())
	if r.manager != nil {
		// Language runtimes frequently need their own scratch files. The Linux
		// sandbox exposes only the Manager temp root and command cwd as writable,
		// so normalize TMPDIR to that same manager-owned boundary.
		cmd.Env = r.manager.FilterEnv(cmd.Env, false)
	}
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

	var stdout, stderr cappedRunCodeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Derive the timeout from the invocation context so stopping a turn or
	// switching sessions cancels the interpreter and its entire process group.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	if err := runCtx.Err(); err != nil {
		return &tools.Result{Output: "RunCode: cancelled before process start", IsError: true}, nil
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("RunCode: start: %s", security.RedactSubprocessText(err.Error()))
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return &tools.Result{
					Output: fmt.Sprintf("exit code %d\nstderr:\n%s\nstdout:\n%s",
						exitErr.ExitCode(),
						security.RedactSubprocessText(strings.TrimSpace(stderr.String())),
						security.RedactSubprocessText(strings.TrimSpace(stdout.String()))),
					IsError: true,
				}, nil
			}
			return nil, fmt.Errorf("RunCode: execute: %s", security.RedactSubprocessText(err.Error()))
		}
	case <-runCtx.Done():
		stopRunCodeProcess(cmd, done, runCodeKillWait)
		prefix := fmt.Sprintf("timed out after %ds", timeoutSec)
		if ctx.Err() != nil {
			prefix = "cancelled by caller"
		}
		return &tools.Result{
			Output: fmt.Sprintf("%s\nstdout:\n%s\nstderr:\n%s",
				prefix,
				security.RedactSubprocessText(strings.TrimSpace(stdout.String())),
				security.RedactSubprocessText(strings.TrimSpace(stderr.String()))),
			IsError: true,
		}, nil
	}

	out := security.RedactSubprocessText(strings.TrimSpace(stdout.String()))
	if stderr.Len() > 0 {
		errOut := security.RedactSubprocessText(strings.TrimSpace(stderr.String()))
		if out != "" {
			out = fmt.Sprintf("stdout:\n%s\n\nstderr:\n%s", out, errOut)
		} else {
			out = fmt.Sprintf("stderr:\n%s", errOut)
		}
	}

	return &tools.Result{Output: out}, nil
}

const runCodeKillWait = 2 * time.Second

var runCodeKillProcessTree = jobs.KillProcessGroup

// stopRunCodeProcess makes cancellation best-effort but bounded. The process
// group kill handles interpreter wrappers and their descendants. A direct
// Process.Kill is deliberately attempted as a second line of defence because
// the Windows taskkill helper (or a Unix process-group kill) can fail. We must
// not then wait on cmd.Wait forever and wedge the entire agent turn.
func stopRunCodeProcess(cmd *exec.Cmd, done <-chan error, wait time.Duration) {
	if wait <= 0 {
		if cmd != nil && cmd.Process != nil {
			treeKiller := runCodeKillProcessTree
			go treeKiller(cmd.Process)
			_ = cmd.Process.Kill()
		}
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	if cmd != nil && cmd.Process != nil {
		treeDone := make(chan struct{})
		treeKiller := runCodeKillProcessTree
		go func() {
			treeKiller(cmd.Process)
			close(treeDone)
		}()
		select {
		case <-treeDone:
			// The platform tree killer completed; still kill the leader directly
			// in case it could not enumerate or signal the process tree.
		case <-done:
			return
		case <-timer.C:
			_ = cmd.Process.Kill()
			return
		}
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-timer.C:
	}
}

const maxRunCodeOutputBytes = 1 << 20

// cappedRunCodeBuffer keeps stdout/stderr from consuming unbounded memory.
// os/exec expects Writer.Write to report the full input length even when the
// retained prefix is capped, otherwise it treats truncation as an I/O error.
type cappedRunCodeBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedRunCodeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := maxRunCodeOutputBytes - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedRunCodeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *cappedRunCodeBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func (b *cappedRunCodeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + "\n[RunCode output truncated]\n"
}
