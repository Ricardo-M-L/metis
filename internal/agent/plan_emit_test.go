package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestEmitPlanRedactsDetachedPresentationAndPreservesEventOrder(t *testing.T) {
	input := map[string]any{
		"path":    "safe/file.go",
		"api_key": "sk-live-plan-secret",
		"command": "curl -H 'Authorization: Bearer plan-command-token' https://example.test",
		"nested": map[string]any{
			"password": "hunter2",
			"label":    "safe-label",
		},
	}
	toolUses := []llm.ContentBlock{{
		Type:      "tool_use",
		ToolUseID: "call-1",
		ToolName:  "Bash",
		ToolInput: input,
	}}
	out := make(chan Event, 2)

	var loop *Loop
	loop.emitPlan(context.Background(), toolUses, out)
	close(out)
	events := make([]Event, 0, 2)
	for event := range out {
		events = append(events, event)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[0].Kind != EventPlan || events[1].Kind != EventLoopDone {
		t.Fatalf("event order = [%v, %v], want [EventPlan, EventLoopDone]", events[0].Kind, events[1].Kind)
	}
	if events[1].StopReason != "plan_mode" {
		t.Fatalf("loop-done stop reason = %q, want plan_mode", events[1].StopReason)
	}
	if len(events[0].ToolCalls) != 1 {
		t.Fatalf("plan tool-call count = %d, want 1", len(events[0].ToolCalls))
	}
	call := events[0].ToolCalls[0]
	if call.ID != "call-1" || call.Name != "Bash" {
		t.Fatalf("plan call identity = (%q, %q), want (call-1, Bash)", call.ID, call.Name)
	}
	presentation := canonicalArgs(call.Input)
	for _, secret := range []string{"sk-live-plan-secret", "plan-command-token", "hunter2"} {
		if strings.Contains(presentation, secret) {
			t.Fatalf("plan presentation leaked %q: %s", secret, presentation)
		}
	}
	for _, want := range []string{"safe/file.go", "safe-label", "[REDACTED]"} {
		if !strings.Contains(presentation, want) {
			t.Fatalf("plan presentation missing safe value %q: %s", want, presentation)
		}
	}

	// Both source preservation and graph detachment matter: a UI consumer may
	// retain or mutate its presentation copy while the raw call remains the
	// execution/canonicalization source.
	call.Input["path"] = "changed-by-consumer"
	callNested, ok := call.Input["nested"].(map[string]any)
	if !ok {
		t.Fatalf("plan nested presentation has unexpected type %T", call.Input["nested"])
	}
	callNested["label"] = "changed-by-consumer"
	if input["path"] != "safe/file.go" {
		t.Fatalf("plan presentation aliases source path: %#v", input)
	}
	sourceNested, ok := input["nested"].(map[string]any)
	if !ok || sourceNested["label"] != "safe-label" || sourceNested["password"] != "hunter2" {
		t.Fatalf("plan presentation aliases or mutates nested source: %#v", input["nested"])
	}
	if input["api_key"] != "sk-live-plan-secret" ||
		input["command"] != "curl -H 'Authorization: Bearer plan-command-token' https://example.test" {
		t.Fatalf("plan redaction mutated raw source: %#v", input)
	}
}
