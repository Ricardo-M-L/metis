package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestToolSearchEmitsStartAndResultWithSameCallIdentity(t *testing.T) {
	stream := toolBatchEvents(llm.ContentBlock{ToolName: "ToolSearch", ToolUseID: "search"})
	stream[1].InputDelta = `{"query":"select:Read"}`
	provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{stream}}
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(planBatchTestTool{name: "Read", gate: gate})
	loop := NewLoop(provider, registry, gate, nil, "system", 5)
	loop.AppendUser("discover the Read schema")
	out := make(chan Event, 128)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var events []Event
	for ev := range out {
		if ev.ToolName == "ToolSearch" && (ev.Kind == EventToolStart || ev.Kind == EventToolResult) {
			events = append(events, ev)
		}
	}
	if len(events) != 2 || events[0].Kind != EventToolStart || events[1].Kind != EventToolResult {
		t.Fatalf("ToolSearch lifecycle = %+v, want start then result", events)
	}
	if events[0].TraceCallID == "" || events[0].TraceCallID != events[1].TraceCallID || events[0].TraceInvocationID != events[1].TraceInvocationID {
		t.Fatalf("ToolSearch call identity changed: %+v", events)
	}
	if events[0].ToolInput["query"] != "select:Read" || events[1].ToolResult == nil || events[1].ToolResult.IsError {
		t.Fatalf("ToolSearch input/result missing: %+v", events)
	}
}
