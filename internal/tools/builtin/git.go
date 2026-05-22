package builtin

import (
	"bytes"
	"context"
	"os/exec"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Git wraps common git operations as tools.
type Git struct {
	tools.BaseTool
	gate *permission.Gate
}

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
	return mapDecision(d), src
}

func (Git) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
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

	cmd := exec.CommandContext(ctx, "git", strings.Fields(args)...)
	if cwd != "" {
		cmd.Dir = cwd
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
