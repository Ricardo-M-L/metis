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
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

type configuredPostHookStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *configuredPostHookStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *configuredPostHookStream) Close() error { return nil }

type configuredPostHookProvider struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (p *configuredPostHookProvider) Name() string          { return "configured-hook-test" }
func (p *configuredPostHookProvider) ModelID() string       { return "configured-hook-model" }
func (p *configuredPostHookProvider) MaxContextTokens() int { return 100_000 }
func (p *configuredPostHookProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream only")
}
func (p *configuredPostHookProvider) Stream(_ context.Context, request llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	call := len(p.requests)
	p.mu.Unlock()
	if call == 1 {
		return &configuredPostHookStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "configured-call", ToolName: "HookLoopTool"},
			{Type: "tool_input_delta", ToolUseID: "configured-call", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "configured-call", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &configuredPostHookStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "done"},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

func (p *configuredPostHookProvider) capturedRequests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

type configuredPostHookTool struct {
	tools.BaseTool
	fail bool
}

func (*configuredPostHookTool) Name() string                { return "HookLoopTool" }
func (*configuredPostHookTool) Description() string         { return "post-hook integration fixture" }
func (*configuredPostHookTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (*configuredPostHookTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*configuredPostHookTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t *configuredPostHookTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	if t.fail {
		return nil, errors.New("fixture execution failed")
	}
	return &tools.Result{Output: "fixture execution passed"}, nil
}

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

func TestLoadConfigHooks_PostToolUseAdditionalContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "flat",
			command: `payload=$(cat); case "$payload" in *'"tool_use_id":"call-42"'*) printf '%s' '{"additional_context":"REGRESSION_SENTINEL"}' ;; *) exit 2 ;; esac`,
			want:    "REGRESSION_SENTINEL",
		},
		{
			name:    "claude envelope",
			command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"ENVELOPE_SENTINEL"}}'`,
			want:    "ENVELOPE_SENTINEL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := pubhook.NewRegistry()
			LoadConfigHooks(reg, &config.HooksConfig{PostToolUse: []config.HookSpec{{
				Type: "command", Command: tc.command, If: "Edit",
			}}})
			in := &pubhook.PostToolUse{
				Tool: "Edit", Input: map[string]any{"path": "x.go"}, Output: "edited", IsError: false,
			}
			hookCtx := pubhook.WithPostToolUseID(context.Background(), "call-42")
			got := reg.EmitPostToolUseContext(hookCtx, pubhook.Context{SessionID: "s", Turn: 2}, in)
			if got != tc.want {
				t.Fatalf("additional context = %q, want %q", got, tc.want)
			}
			if off := reg.EmitPostToolUseContext(context.Background(), pubhook.Context{}, &pubhook.PostToolUse{Tool: "Read"}); strings.Contains(off, "SENTINEL") {
				t.Fatalf("non-matching tool received feedback: %q", off)
			}
		})
	}
}

func TestConfiguredPostToolUseFeedbackReachesNextModelRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	for _, failTool := range []bool{false, true} {
		name := "success"
		if failTool {
			name = "go error"
		}
		t.Run(name, func(t *testing.T) {
			hooks := pubhook.NewRegistry()
			LoadConfigHooks(hooks, &config.HooksConfig{PostToolUse: []config.HookSpec{{
				Type:    "command",
				If:      "HookLoopTool",
				Command: `payload=$(cat); case "$payload" in *'"tool_use_id":"configured-call"'*) printf '%s' '{"additional_context":"CONFIGURED_LOOP_SENTINEL"}' ;; *) exit 2 ;; esac`,
			}}})
			registry := tools.NewRegistry()
			registry.Register(&configuredPostHookTool{fail: failTool})
			provider := &configuredPostHookProvider{}
			loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypass), hooks, "sys", 10)
			loop.AppendUser("run the fixture")

			events := make(chan agent.Event, 64)
			if err := loop.Run(context.Background(), events); err != nil {
				t.Fatal(err)
			}
			close(events)
			requests := provider.capturedRequests()
			if len(requests) != 2 {
				t.Fatalf("provider calls = %d, want 2", len(requests))
			}
			var resultBody string
			for _, message := range requests[1].Messages {
				for _, block := range message.Content {
					if block.Type == "tool_result" && block.ToolUseID == "configured-call" {
						resultBody = block.ToolResult
					}
				}
			}
			if !strings.Contains(resultBody, "CONFIGURED_LOOP_SENTINEL") || !strings.Contains(resultBody, "system-reminder") {
				t.Fatalf("configured hook feedback missing from next request: %q", resultBody)
			}
			if failTool && !strings.Contains(resultBody, "fixture execution failed") {
				t.Fatalf("Go error was lost when feedback was injected: %q", resultBody)
			}
		})
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
	if res.Output == nil || !res.Output.IsError {
		t.Fatalf("exit 49 must also block the current tool, got %+v", res)
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
