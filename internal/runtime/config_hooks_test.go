package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestRunHookCommandUsesRestrictedEnvironment(t *testing.T) {
	t.Setenv("OPA_TOKEN", "must-not-reach-hook")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-hook-either")
	out, err := runHookCommand(context.Background(), config.HookSpec{
		Command: `printf '%s|%s|%s' "${OPA_TOKEN-unset}" "${ANTHROPIC_API_KEY-unset}" "${METIS_HOOK_EVENT-unset}"`,
	}, map[string]any{"hook_event_name": "SessionStart"})
	if err != nil {
		t.Fatalf("runHookCommand: %v", err)
	}
	if got := string(out); got != "unset|unset|SessionStart" {
		t.Fatalf("hook environment crossed the credential boundary: %q", got)
	}
}

func TestRunHookCommandRedactsModelVisibleOutput(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	out, err := runHookCommand(context.Background(), config.HookSpec{
		Command: `printf '%s' '{"decision":"deny","reason":"token ghp_abcdefghijklmnopqrstuvwxyz1234567890"}'`,
	}, map[string]any{"hook_event_name": "PreToolUse"})
	if err != nil {
		t.Fatalf("runHookCommand: %v", err)
	}
	if strings.Contains(string(out), secret) || !strings.Contains(string(out), "[REDACTED]") {
		t.Fatalf("hook output was not redacted: %s", out)
	}
	mod := parsePreToolUseResponse(out)
	if mod == nil || mod.Output == nil || !mod.Output.IsError || strings.Contains(mod.Output.Content, secret) {
		t.Fatalf("redacted hook JSON no longer parsed safely: %#v", mod)
	}
}

func TestRunHookCommandOrdinaryAuthMetadataKeepsDenyJSONValid(t *testing.T) {
	out, err := runHookCommand(context.Background(), config.HookSpec{
		Command: `printf '%s' '{"decision":"deny","reason":"auth=required","author":"Alice","token_count":42}'`,
	}, map[string]any{"hook_event_name": "PreToolUse"})
	if err != nil {
		t.Fatalf("runHookCommand: %v", err)
	}
	mod := parsePreToolUseResponse(out)
	if mod == nil || mod.Output == nil || !mod.Output.IsError || mod.Output.Content != "auth=required" {
		t.Fatalf("ordinary metadata was corrupted or deny failed open: output=%s parsed=%#v", out, mod)
	}
}

func TestRedactStructuredHookOutputDecodesEscapedSecretsRecursively(t *testing.T) {
	secret := "quote\" backslash\\ newline\nsecret"
	env := []string{"HOOK_TOKEN=" + secret}
	raw, err := json.Marshal(map[string]any{
		"output": secret,
		"nested": map[string]any{
			"values": []any{"safe", map[string]any{"reason": "prefix " + secret + " suffix"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The encoded representation contains escaped quote/backslash/newline
	// sequences, so replacing the decoded environment value in raw bytes would
	// miss it. The production helper must decode before redacting.
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("fixture did not exercise JSON escaping")
	}

	redacted := redactStructuredHookOutput(raw, env)
	var got map[string]any
	if err := json.Unmarshal(redacted, &got); err != nil {
		t.Fatalf("redacted response is invalid JSON: %v; %s", err, redacted)
	}
	encoded := string(redacted)
	if strings.Contains(encoded, "quote") || strings.Contains(encoded, "backslash") || strings.Contains(encoded, "secret") {
		t.Fatalf("escaped secret survived recursive redaction: %s", encoded)
	}
	if strings.Count(encoded, "[REDACTED]") != 2 {
		t.Fatalf("nested string values were not all redacted: %s", encoded)
	}
}

func TestStructuredModelVisibleHookParsersReceiveRedactedStrings(t *testing.T) {
	secret := "quote\" backslash\\ newline\nsecret"
	env := []string{"HOOK_TOKEN=" + secret}
	tests := []struct {
		name    string
		body    map[string]any
		extract func([]byte) string
	}{
		{
			name: "user_prompt_output",
			body: map[string]any{"output": "assistant " + secret, "modified_prompt": "prompt " + secret},
			extract: func(out []byte) string {
				mod := parseUserPromptResponse(out)
				if mod == nil || mod.Output == nil {
					t.Fatal("user-prompt response did not parse")
				}
				return mod.Output.Content + "\n" + mod.ModifiedPrompt
			},
		},
		{
			name: "permission_reason",
			body: map[string]any{"decision": "deny", "reason": "policy " + secret},
			extract: func(out []byte) string {
				mod := parsePermissionResponse(out)
				if mod == nil {
					t.Fatal("permission response did not parse")
				}
				return mod.Reason
			},
		},
		{
			name: "post_compact_context",
			body: map[string]any{"additional_context": "context " + secret},
			extract: func(out []byte) string {
				mod := parsePostCompactResponse(out)
				if mod == nil {
					t.Fatal("post-compact response did not parse")
				}
				return mod.AdditionalContext
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			visible := tc.extract(redactStructuredHookOutput(raw, env))
			if strings.Contains(visible, secret) || !strings.Contains(visible, "[REDACTED]") {
				t.Fatalf("model-visible response was not redacted: %q", visible)
			}
		})
	}
}

func TestFormatHookCommandErrorRedactsCommandAndError(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	line := formatHookCommandError(
		"PreToolUse",
		config.HookSpec{Command: "policy --token " + secret},
		map[string]any{"hook_event_name": "PreToolUse"},
		errors.New("failed with "+secret),
	)
	if strings.Contains(line, secret) || strings.Count(line, "[REDACTED]") < 2 {
		t.Fatalf("hook error log leaked command/error credential: %q", line)
	}
}

func TestPreToolUseModifiedInputPreservesInternallyInjectedCredential(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	reg := pubhook.NewRegistry()
	LoadConfigHooks(reg, &config.HooksConfig{PreToolUse: []config.HookSpec{{
		Type:    "command",
		Command: fmt.Sprintf(`printf '%%s' '{"modified_input":{"api_key":"%s"}}'`, secret),
	}}})

	res := reg.EmitPreToolUse(context.Background(), pubhook.Context{},
		&pubhook.PreToolUse{Tool: "McpTool", Input: map[string]any{}})
	if res == nil || res.ModifiedInput == nil || res.ModifiedInput["api_key"] != secret {
		t.Fatalf("hook credential injection semantics changed: %#v", res)
	}
	if res.PresentationInput == nil || res.PresentationInput["api_key"] != "[REDACTED]" {
		t.Fatalf("hook presentation input was not independently redacted: %#v", res)
	}
}

func TestPreToolUseRedactsReasonAfterControlJSONParsing(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	reg := pubhook.NewRegistry()
	LoadConfigHooks(reg, &config.HooksConfig{PreToolUse: []config.HookSpec{{
		Type:    "command",
		Command: fmt.Sprintf(`printf '%%s' '{"decision":"deny","reason":"policy token %s"}'`, secret),
	}}})

	res := reg.EmitPreToolUse(context.Background(), pubhook.Context{},
		&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "echo ok"}})
	if res == nil || res.Output == nil || !res.Output.IsError {
		t.Fatalf("deny control response lost: %#v", res)
	}
	if strings.Contains(res.Output.Content, secret) || !strings.Contains(res.Output.Content, "[REDACTED]") {
		t.Fatalf("control reason was not redacted after parsing: %q", res.Output.Content)
	}
}

func TestPreToolUseNonZeroExitCannotFailOpen(t *testing.T) {
	for _, code := range []int{2, 7} {
		t.Run(fmt.Sprintf("exit_%d", code), func(t *testing.T) {
			reg := pubhook.NewRegistry()
			LoadConfigHooks(reg, &config.HooksConfig{PreToolUse: []config.HookSpec{{
				Type:    "command",
				Command: fmt.Sprintf("exit %d", code),
			}}})
			res := reg.EmitPreToolUse(context.Background(), pubhook.Context{},
				&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "echo must-not-run"}})
			if res == nil || res.Output == nil || !res.Output.IsError {
				t.Fatalf("exit %d failed open: %#v", code, res)
			}
		})
	}
}

func TestPreToolUseMalformedOrUnknownJSONCannotFailOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"decision":`},
		{name: "unknown_shape", body: `{"status":"ok"}`},
		{name: "unknown_decision", body: `{"decision":"maybe"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := pubhook.NewRegistry()
			LoadConfigHooks(reg, &config.HooksConfig{PreToolUse: []config.HookSpec{{
				Type:    "command",
				Command: fmt.Sprintf("printf '%%s' '%s'", tc.body),
			}}})
			res := reg.EmitPreToolUse(context.Background(), pubhook.Context{},
				&pubhook.PreToolUse{Tool: "Bash", Input: map[string]any{"command": "echo must-not-run"}})
			if res == nil || res.Output == nil || !res.Output.IsError {
				t.Fatalf("successful hook with %s JSON failed open: %#v", tc.name, res)
			}
			if !strings.Contains(res.Output.Content, "malformed or unknown") {
				t.Fatalf("unexpected fail-closed reason: %#v", res)
			}
		})
	}
}

func TestCappedHookStderrBoundsMemory(t *testing.T) {
	var b cappedHookStderr
	payload := strings.Repeat("x", maxHookStderrBytes+1024)
	if n, err := b.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if b.Len() != maxHookStderrBytes || !b.Truncated() {
		t.Fatalf("capped stderr len=%d truncated=%v", b.Len(), b.Truncated())
	}
	if !strings.Contains(b.String(), "[hook stderr truncated]") {
		t.Fatalf("truncation marker missing")
	}
}

func TestCappedHookStdoutBoundsMemory(t *testing.T) {
	var b cappedHookStdout
	payload := strings.Repeat("x", maxHookStdoutBytes+1024)
	if n, err := b.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if len(b.String()) != maxHookStdoutBytes || !b.Truncated() {
		t.Fatalf("capped stdout len=%d truncated=%v", len(b.String()), b.Truncated())
	}
}

func TestRunHookCommandRejectsTruncatedStdout(t *testing.T) {
	_, _, err := runHookCommandWithCode(context.Background(), config.HookSpec{
		Command: `head -c 1100000 /dev/zero | tr '\0' x`,
		Timeout: 2,
	}, map[string]any{"hook_event_name": "PreToolUse"})
	if err == nil || !strings.Contains(err.Error(), "stdout exceeded") {
		t.Fatalf("oversized hook stdout error = %v, want explicit cap error", err)
	}
}

func TestRunHookCommandTimeoutKillsBackgroundProcessTree(t *testing.T) {
	started := time.Now()
	_, _, err := runHookCommandWithCode(context.Background(), config.HookSpec{
		Command: `sleep 999 & wait`,
		Timeout: 1,
	}, map[string]any{"hook_event_name": "PreToolUse"})
	if err == nil {
		t.Fatal("background hook unexpectedly completed without timeout")
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("hook timeout waited on orphaned child for %s", elapsed)
	}
}

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
