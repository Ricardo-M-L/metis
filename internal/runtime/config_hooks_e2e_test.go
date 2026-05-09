package runtime

// config_hooks_e2e_test.go runs real subprocess hooks (sh -c) to
// confirm the end-to-end claude-code parity:
//
//   - exit code 49 raises Halt without needing JSON on stdout
//   - JSON `{"decision":"halt"}` raises Halt with a reason
//   - claude-code envelope format works end-to-end (the JSON
//     parsing is unit-tested separately; this confirms it's
//     reachable from the subprocess path)
//
// These run actual `sh -c` invocations, so they're slightly more
// expensive than the parser-only tests in config_hooks_halt_test.go —
// kept in a separate file to make that fact explicit.

import (
	"context"
	"runtime"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestLoadConfigHooks_PreToolUseHaltViaJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c not available on Windows runner")
	}
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type:    "command",
				Command: `echo '{"decision":"halt","reason":"compliance violation"}'`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{SessionID: "s-halt"},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "rm -rf /"}})
	if res == nil {
		t.Fatal("expected non-nil ModifiedPreToolUse")
	}
	if !res.Halt {
		t.Errorf("Halt should be true; got %+v", res)
	}
	if res.HaltReason != "compliance violation" {
		t.Errorf("HaltReason = %q, want %q", res.HaltReason, "compliance violation")
	}
	if res.Output == nil {
		t.Errorf("halt should also synthesize an Output for the dangling tool_use")
	}
}

// TestLoadConfigHooks_PreToolUseHaltViaExit49 — the "I don't want to
// emit JSON" form: a one-line shell hook can `exit 49` and metis must
// still recognize halt. claude-code's documented convention.
func TestLoadConfigHooks_PreToolUseHaltViaExit49(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c not available on Windows runner")
	}
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type:    "command",
				Command: `exit 49`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{SessionID: "s-49"},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "x"}})
	if res == nil || !res.Halt {
		t.Fatalf("exit 49 should raise Halt; got %+v", res)
	}
	if res.HaltReason == "" {
		t.Error("exit-49 path should set a default HaltReason for the transcript")
	}
}

// TestLoadConfigHooks_PreToolUseClaudeEnvelopeEndToEnd — confirms the
// claude-code stdout envelope is parseable through the real
// subprocess path, not just the unit parser. Drop-in compatibility
// with users' existing claude-code hooks is the headline feature.
func TestLoadConfigHooks_PreToolUseClaudeEnvelopeEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh -c not available on Windows runner")
	}
	reg := pubhook.NewRegistry()
	cfg := &config.HooksConfig{
		PreToolUse: []config.HookSpec{
			{
				Type: "command",
				Command: `cat <<'EOF'
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"sentinel"}}
EOF`,
			},
		},
	}
	LoadConfigHooks(reg, cfg)

	res := reg.EmitPreToolUse(context.Background(),
		pubhook.Context{SessionID: "s-env"},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "x"}})
	if res == nil || res.Output == nil {
		t.Fatalf("envelope deny should produce Output; got %+v", res)
	}
	if res.Output.Content != "sentinel" {
		t.Errorf("envelope reason not propagated; got %q", res.Output.Content)
	}
}
