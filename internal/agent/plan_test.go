package agent

import (
	"strings"
	"testing"
)

func TestPlan_Render(t *testing.T) {
	p := &Plan{
		Text: "1. Read file\n2. Edit file\n3. Write back",
		ToolCalls: []ToolCall{
			{ID: "c1", Name: "Read", Input: map[string]any{"path": "/tmp/foo.txt"}},
			{ID: "c2", Name: "Edit", Input: map[string]any{"path": "/tmp/foo.txt", "old": "foo", "new": "bar"}},
		},
		Summary: "Update foo → bar in /tmp/foo.txt",
	}
	out := p.Render()
	if !strings.Contains(out, "## Plan") {
		t.Error("missing plan header")
	}
	if !strings.Contains(out, "Read") {
		t.Error("missing Read tool")
	}
	if !strings.Contains(out, "foo → bar") {
		t.Error("missing summary")
	}
	if !strings.Contains(out, "Proceed?") {
		t.Error("missing prompt")
	}
}

func TestCollectToolCallsFromEvents(t *testing.T) {
	events := []Event{
		{Kind: EventTextDelta, TextDelta: "hello"},
		{Kind: EventPlan, ToolCalls: []ToolCall{
			{ID: "c1", Name: "LS", Input: map[string]any{"path": "/tmp"}},
		}},
		{Kind: EventLoopDone, StopReason: "plan_mode"},
	}
	calls := CollectToolCallsFromEvents(events)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "LS" {
		t.Errorf("want LS, got %s", calls[0].Name)
	}
}
