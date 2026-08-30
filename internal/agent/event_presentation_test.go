package agent

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestEventPresentationCopyDropsAuthorizationState(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"api_key": "raw-secret",
		"nested":  []any{map[string]any{"password": "nested-secret"}},
		"safe":    "keep",
	}
	ev := permissionRequestEvent(llm.ContentBlock{
		ToolUseID: "tool-1",
		ToolName:  "CredentialTool",
		ToolInput: raw,
	}, redactedToolInput(raw), 0)
	ev.AskUserReply = make(chan string, 1)
	ev.ToolCalls = []ToolCall{{ID: "plan-1", Name: "CredentialTool", Input: raw}}
	ev.ToolResult = &ToolResult{Presentation: raw}

	presentation := ev.PresentationCopy()
	if _, ok := presentation.PermissionPolicyInputForAuthorization(); ok {
		t.Fatal("presentation copy retained raw authorization input")
	}
	if presentation.PermissionReply != nil || presentation.AskUserReply != nil {
		t.Fatal("presentation copy retained an interactive reply channel")
	}
	if got := presentation.ToolInput["api_key"]; got != "[REDACTED]" {
		t.Fatalf("tool input api_key = %#v, want redacted", got)
	}
	if got := presentation.PermissionInput["api_key"]; got != "[REDACTED]" {
		t.Fatalf("permission input api_key = %#v, want redacted", got)
	}
	if got := presentation.ToolCalls[0].Input["api_key"]; got != "[REDACTED]" {
		t.Fatalf("plan tool input api_key = %#v, want redacted", got)
	}
	if got := presentation.ToolResult.Presentation["api_key"]; got != "[REDACTED]" {
		t.Fatalf("tool result presentation api_key = %#v, want redacted", got)
	}

	policyInput, ok := ev.PermissionPolicyInputForAuthorization()
	if !ok || policyInput["api_key"] != "raw-secret" {
		t.Fatalf("source authorization input = %#v, %v; want original raw snapshot", policyInput, ok)
	}
	presentation.ToolInput["safe"] = "changed"
	presentation.ToolCalls[0].Input["safe"] = "changed"
	if ev.ToolInput["safe"] != "keep" || ev.ToolCalls[0].Input["safe"] != "keep" {
		t.Fatal("presentation copy aliases the source event")
	}
}
