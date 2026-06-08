package builtin

// slash_command.go — the SlashCommand tool. Lets the model invoke a
// user-authored slash command (a "recipe" from ~/.metis/commands/*.md)
// by name and receive its expanded body as the tool result. Mirrors
// claude-code's SlashCommand tool.
//
// Decoupled from internal/slash via the SlashRunner interface: importing
// the slash package here would cycle (builtin → slash → runtime →
// builtin). cmd/metis supplies a concrete runner that wraps the live
// slash.Registry and enforces the "custom commands only" rule — built-in
// TUI commands (quit/clear/compact/…) return control Signals a tool
// can't honor, so the runner refuses them.

import (
	"context"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// SlashRunner dispatches a user-authored slash command on the model's
// behalf. Implemented by cmd/metis over the live slash.Registry.
type SlashRunner interface {
	// RunForModel runs `command` (e.g. "/standup api" — leading slash
	// optional) and returns the expanded text. Returns an error for an
	// unknown command or one that isn't model-invokable (a built-in
	// TUI command).
	RunForModel(command string) (string, error)
	// Names lists the invokable (custom) command names, for the tool
	// description so the model knows what's available.
	Names() []string
}

// SlashCommand is the model-facing tool. Nil runner disables it
// gracefully (Execute returns a clear error) — same nil-safety pattern
// as the other late-wired tools.
type SlashCommand struct {
	tools.BaseTool
	gate   *permission.Gate
	runner SlashRunner
}

// NewSlashCommand builds the tool. Pass the cmd/metis runner adapter.
func NewSlashCommand(gate *permission.Gate, runner SlashRunner) SlashCommand {
	return SlashCommand{gate: gate, runner: runner}
}

func (SlashCommand) Name() string { return "SlashCommand" }

func (s SlashCommand) Description() string {
	base := "Invoke a user-authored slash command (a saved recipe from ~/.metis/commands/*.md) by name and get its expanded body back. The recipe's $ARGUMENTS/$1 substitutions, !`cmd` shell injections, and @file includes are all expanded. Use this to run the user's saved workflows. Only user-defined custom commands are invokable — built-in TUI commands (/clear, /compact, etc.) are not."
	if s.runner != nil {
		if names := s.runner.Names(); len(names) > 0 {
			base += " Available: " + strings.Join(names, ", ") + "."
		}
	}
	return base
}

func (SlashCommand) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"command"},
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The slash command to run, with optional args, e.g. \"/standup api\" or \"standup api\". Must be a user-defined custom command.",
			},
		},
	}
}

func (SlashCommand) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (s SlashCommand) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := s.gate.Check(context.Background(), "SlashCommand", strFromAny(in["command"]))
	return mapDecision(d), src
}

func (s SlashCommand) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if s.runner == nil {
		return &tools.Result{Output: "SlashCommand unavailable: no slash registry wired in this session", IsError: true}, nil
	}
	cmd, _ := in["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return &tools.Result{Output: "`command` is required (e.g. \"/standup\")", IsError: true}, nil
	}
	out, err := s.runner.RunForModel(cmd)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: out}, nil
}
