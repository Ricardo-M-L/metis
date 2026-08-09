package builtin

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Git wraps common git operations as tools.
type Git struct {
	tools.BaseTool
	gate    *permission.Gate
	sandbox *sandbox.Manager
}

// NewGit constructs the legacy unsandboxed Git tool. Runtime construction
// should use NewGitWithSandbox so Git shares Bash and Workflow's Manager.
func NewGit(gate *permission.Gate) Git { return Git{gate: gate} }

// NewGitWithSandbox constructs Git with a runtime-owned sandbox Manager.
func NewGitWithSandbox(gate *permission.Gate, manager *sandbox.Manager) Git {
	return Git{gate: gate, sandbox: manager}
}

// WithSandbox returns a copy wired to manager. Git is a value tool in the
// registry, so this mirrors Bash.WithSandbox.
func (g Git) WithSandbox(manager *sandbox.Manager) Git {
	g.sandbox = manager
	return g
}

// SandboxManager returns the Manager applied to git subprocesses.
func (g Git) SandboxManager() *sandbox.Manager { return g.sandbox }

func (Git) Name() string        { return "Git" }
func (Git) Description() string { return "Run a git command. Wrapper around common git operations." }
func (Git) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"args"},
		"properties": map[string]any{
			"args": map[string]any{"type": "string", "description": "git arguments (e.g. 'status', 'log --oneline -5')"},
			"cwd":  map[string]any{"type": "string", "description": "working directory"},
			"env":  map[string]any{"type": "object", "description": "extra env vars"},
		},
	}
}
func (Git) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }
func (g Git) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	args, _ := in["args"].(string)
	d, src := g.gate.Check(context.Background(), "Git", args)
	if d == permission.DecisionAsk && g.sandbox != nil && g.sandbox.AutoAllow() {
		return tools.PermissionAllow, "sandbox auto-allow"
	}
	return mapDecision(d), src
}

func (g Git) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	args, _ := in["args"].(string)
	if args == "" {
		// 2026-05-22: rich error replacing bare errors.New. Common
		// confusions: model passed `command` or `cmd` thinking it
		// was the field name, or sub-command alone like just
		// "status" needs "args".
		hint := ""
		if c, _ := in["command"].(string); c != "" {
			hint = "\n\nYou passed `command`. The argument name is `args`. Try Git({args: \"" + c + "\"})."
		} else if c, _ := in["cmd"].(string); c != "" {
			hint = "\n\nYou passed `cmd`. The argument name is `args`. Try Git({args: \"" + c + "\"})."
		}
		return &tools.Result{
			Output:  "Git: `args` is required (e.g. \"status\", \"diff HEAD~1\", \"log --oneline -10\"). The args string is everything that would follow `git` on the command line." + hint,
			IsError: true,
		}, nil
	}
	cwd, _ := in["cwd"].(string)
	if cwd == "" {
		cwd = agent.CwdFromContext(ctx)
	}

	cmd := exec.CommandContext(ctx, "git", strings.Fields(args)...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if g.sandbox != nil {
		// Git is model-controlled too; never inherit provider credentials by
		// default merely because it is a dedicated tool instead of Bash.
		cmd.Env = g.sandbox.FilterEnv(os.Environ(), false)
	}
	if g.sandbox != nil && g.sandbox.EffectiveMode() != sandbox.ModeOff {
		tempDir := g.sandbox.TempDir()
		if tempDir != "" {
			base := cmd.Env
			if base == nil {
				base = os.Environ()
			}
			cmd.Env = setGitTempEnv(base, tempDir)
		}
	}
	if g.sandbox != nil {
		wrapped, err := g.sandbox.Wrap(cmd, sandbox.Request{Cwd: cmd.Dir})
		if err != nil {
			return &tools.Result{Output: "Git: sandbox wrap failed: " + err.Error(), IsError: true}, nil
		}
		cmd = wrapped
	}
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		out := stdout.String()
		if stderr.Len() > 0 {
			out += "\n" + stderr.String()
		}
		return &tools.Result{Output: strings.TrimSpace(out), IsError: true}, nil
	}
	return &tools.Result{Output: strings.TrimSpace(stdout.String())}, nil
}

func setGitTempEnv(env []string, tempDir string) []string {
	out := make([]string, 0, len(env)+3)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			switch strings.ToUpper(name) {
			case "TMPDIR", "TMP", "TEMP":
				continue
			}
		}
		out = append(out, entry)
	}
	return append(out, "TMPDIR="+tempDir, "TMP="+tempDir, "TEMP="+tempDir)
}
