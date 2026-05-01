package runtime

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestLoadConfigHooks_PreToolUseDeny(t *testing.T) {
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type:    "command",
				If:      "Bash",
				Command: `echo '{"decision":"deny","reason":"git commits forbidden"}'`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{SessionID: "s1", Model: "m"},
		&pubhook.PreToolUse{
			Context: pubhook.Context{SessionID: "s1"},
			Tool:    "Bash",
			Input:   map[string]any{"command": "git push"},
		})
	if res == nil || res.Output == nil || !res.Output.IsError {
		t.Fatalf("hook should deny tool call, got %#v", res)
	}
	if res.Output.Content == "" {
		t.Errorf("expected reason text, got empty")
	}
}

func TestLoadConfigHooks_PreToolUseModifyInput(t *testing.T) {
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type:    "command",
				Command: `echo '{"modified_input":{"command":"ls -la"}}'`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "ls"}})
	if res == nil || res.ModifiedInput == nil {
		t.Fatalf("expected modified_input, got %#v", res)
	}
	if got := res.ModifiedInput["command"]; got != "ls -la" {
		t.Errorf("rewrite missing/wrong: %v", got)
	}
}

func TestLoadConfigHooks_IfFiltersByTool(t *testing.T) {
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type:    "command",
				If:      "Bash",
				Command: `echo '{"decision":"deny"}'`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	// `Read` should NOT trigger the Bash-only hook.
	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{},
		&pubhook.PreToolUse{Tool: "Read", Input: map[string]any{"path": "/tmp/x"}})
	if res != nil {
		t.Errorf("hook with If=Bash fired on Read: %#v", res)
	}
}

func TestLoadConfigHooks_GlobMatchPrefix(t *testing.T) {
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type:    "command",
				If:      "Bash(git *)",
				Command: `echo '{"decision":"deny","reason":"no git"}'`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	// Bash with non-git command should NOT be denied.
	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "ls"}})
	if res != nil {
		t.Errorf("hook fired on non-matching Bash command: %#v", res)
	}

	// Bash with git command SHOULD be denied.
	res2 := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "git push"}})
	if res2 == nil || res2.Output == nil || !res2.Output.IsError {
		t.Errorf("hook should deny `git push`, got %#v", res2)
	}
}

func TestLoadConfigHooks_NilInputsAreSafe(t *testing.T) {
	// Nil registry / config must not panic.
	LoadConfigHooks(nil, nil)
	LoadConfigHooks(pubhook.NewRegistry(), nil)
	var cfg config.HooksConfig
	LoadConfigHooks(pubhook.NewRegistry(), &cfg)
}
