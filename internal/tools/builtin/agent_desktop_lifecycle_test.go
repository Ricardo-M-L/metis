package builtin

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentDesktopLifecycleIsCorrelatedAndDurable(t *testing.T) {
	var traced traceCapture
	agent.SetTraceHook(traced.add)
	t.Cleanup(func() { agent.SetTraceHook(nil) })
	dir := t.TempDir()
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system").
		WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "parent-session")
	parentEvents := make(chan agent.Event, 32)
	ctx := agent.WithEventOut(context.Background(), parentEvents)
	ctx = agent.WithParentToolUseID(ctx, "parent-tool-use")
	ctx = tools.WithInvocationID(ctx, "parent-trace-call")
	ctx = agent.WithTraceInvocationID(ctx, "parent-trace-invocation")
	result, err := tool.Execute(ctx, map[string]any{"prompt": "inspect", "name": "auditor"})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Agent.Execute = %+v, %v", result, err)
	}
	meta, ok := result.Presentation["subagent"].(map[string]any)
	if !ok {
		t.Fatalf("missing durable subagent presentation: %#v", result.Presentation)
	}
	if meta["sessionId"] != "parent-session" || meta["parentToolUseId"] != "parent-tool-use" || meta["name"] != "auditor" || meta["status"] != "completed" {
		t.Fatalf("wrong subagent presentation: %#v", meta)
	}
	agentID, _ := meta["agentId"].(string)
	if agentID == "" {
		t.Fatalf("no stable agent ID in presentation: %#v", meta)
	}
	var seenStart, seenEnd bool
	for len(parentEvents) > 0 {
		ev := <-parentEvents
		if ev.Kind != agent.EventSubAgentStart && ev.Kind != agent.EventSubAgentEnd {
			if ev.Kind == agent.EventTextDelta {
				t.Fatal("child TextDelta leaked into parent")
			}
			continue
		}
		if ev.SubAgentParentID != "parent-tool-use" || ev.SubAgentID != agentID || ev.TraceCallID != "parent-trace-call" || ev.TraceInvocationID != "parent-trace-invocation" {
			t.Fatalf("uncorrelated lifecycle event: %+v", ev)
		}
		if ev.Kind == agent.EventSubAgentStart {
			seenStart = ev.SubAgentStatus == "running"
		} else {
			seenEnd = ev.SubAgentStatus == "completed"
		}
	}
	if !seenStart || !seenEnd {
		t.Fatalf("missing start/end: start=%v end=%v", seenStart, seenEnd)
	}
	seenStart, seenEnd = false, false
	for _, ev := range traced.snapshot() {
		if ev.TraceInvocationID != "parent-trace-invocation" || ev.SubAgentParentID != "parent-tool-use" || ev.SubAgentID != agentID {
			continue
		}
		seenStart = seenStart || ev.Kind == agent.EventSubAgentStart
		seenEnd = seenEnd || ev.Kind == agent.EventSubAgentEnd
	}
	if !seenStart || !seenEnd {
		t.Fatalf("trace hook lost child lifecycle: start=%v end=%v", seenStart, seenEnd)
	}
	header, err := agent.LoadSubAgentHeader(dir, agentID)
	if err != nil || header.SubAgentOf != "parent-session" {
		t.Fatalf("durable ownership = %+v, %v", header, err)
	}
	snapshot, err := agent.LoadSubAgentSnapshot(dir, agentID)
	if err != nil || snapshot.Terminal == nil || snapshot.Terminal.Status != "completed" || snapshot.Terminal.Result != "sub-agent done" {
		t.Fatalf("durable terminal = %+v, %v", snapshot, err)
	}
}
