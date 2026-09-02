package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type recoveredNativeTool struct {
	tools.BaseTool
	calls *atomic.Int32
}

func (recoveredNativeTool) Name() string        { return "RecoveredNative" }
func (recoveredNativeTool) Description() string { return "test native recovery" }
func (recoveredNativeTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (recoveredNativeTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (recoveredNativeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t recoveredNativeTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	t.calls.Add(1)
	return &tools.Result{Output: "RECOVERED_TOOL_OK"}, nil
}

const plainTextRecoveredCall = `<tool_call>
<function=RecoveredNative>
<parameter=value>same arguments</parameter>
</function>
</tool_call>`

func TestSubAgentRetriesPlainTextToolCallThroughNativeInterface(t *testing.T) {
	var calls atomic.Int32
	registry := tools.NewRegistry()
	registry.Register(recoveredNativeTool{calls: &calls})
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		textStream(plainTextRecoveredCall),
		&mockStream{events: toolBatchEvents(llm.ContentBlock{
			Type: "tool_use", ToolUseID: "native-retry-1", ToolName: "RecoveredNative",
		})},
		textStream("RECOVERY_FINAL_OK"),
	}}
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 5)
	loop.RecoverTextToolCalls = true
	loop.AppendUser("use the tool")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)
	if calls.Load() != 1 {
		t.Fatalf("native tool calls = %d, want 1", calls.Load())
	}
	requests := provider.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("provider calls = %d, want text retry + native tool + final", len(requests))
	}
	if !requestContains(requests[1], "no tool was executed") ||
		!requestContains(requests[1], "native structured tool-call interface") {
		t.Fatal("native retry request is missing the recovery reminder")
	}
	if got := assistantText(filterAssistantBlocks(loop.History())); !strings.Contains(got, "RECOVERY_FINAL_OK") {
		t.Fatalf("history missing final recovery marker: %q", got)
	}
}

func TestPlainTextToolCallRecoveryIsOptInAndNeverExecutesText(t *testing.T) {
	var calls atomic.Int32
	registry := tools.NewRegistry()
	registry.Register(recoveredNativeTool{calls: &calls})
	provider := &queuedStreamProvider{streams: []llm.StreamReader{textStream(plainTextRecoveredCall)}}
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 2)
	loop.AppendUser("show a textual example")

	out := make(chan Event, 16)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)
	if calls.Load() != 0 {
		t.Fatalf("plain text was executed %d time(s)", calls.Load())
	}
	if len(provider.capturedRequests()) != 1 {
		t.Fatal("top-level loop unexpectedly retried textual tool markup")
	}
}
