package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceRequestContextFirstRootOnlyAndImmutable(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := NewTraceAdapter(store)
	a.SetSession("context")
	a.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "child", TraceInvocationID: "child-invocation"})
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, TraceInvocationID: "child-invocation", SubAgentParentID: "child", RequestContext: &agent.TraceRequestContext{System: "child prompt"}})
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
	request := &agent.TraceRequestContext{System: "assembled system\nAuthorization: Bearer " + strings.Repeat("A", 30),
		Tools: []llm.ToolSpec{{Name: "Read", Description: "read exact files", InputSchema: schema}}, Model: "actual-model", Provider: "fixture"}
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, TraceStreamID: "stream-one", RequestContext: request})
	schema["type"] = "mutated"
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, TraceStreamID: "stream-two", RequestContext: &agent.TraceRequestContext{System: "later changed system"}})
	// A new adapter resumes the same persisted session; it must not replace or
	// duplicate the original snapshot with a resumed request's prompt.
	b := NewTraceAdapter(store)
	b.SetSession("context")
	b.OnEvent(agent.Event{Kind: agent.EventRequestContext, RequestContext: &agent.TraceRequestContext{System: "resumed"}})
	var snapshots []*session.TraceRequestContext
	for _, ev := range store.Events("context") {
		if ev.RequestContext != nil {
			snapshots = append(snapshots, ev.RequestContext)
		}
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count=%d", len(snapshots))
	}
	got := snapshots[0]
	if !got.Available || got.Source != "logical-request" || got.Model != "actual-model" || got.Turn != 1 || strings.Contains(got.System, strings.Repeat("A", 30)) || !strings.Contains(got.System, "[REDACTED]") {
		t.Fatalf("wrong root context: %+v", got)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "Read" || got.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("schema changed with request: %+v", got.Tools)
	}
	if got.RequestID != "" {
		t.Fatal("stream identity must not masquerade as transport request_id")
	}
}

func TestTraceRequestContextLegacyCaptureKeepsLaterTurnAndBounds(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Append(&session.TraceEvent{SessionID: "legacy", Turn: 7, Kind: "text", Text: "old answer"}); err != nil {
		t.Fatal(err)
	}
	a := NewTraceAdapter(store)
	a.SetSession("legacy")
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, RequestContext: &agent.TraceRequestContext{System: strings.Repeat("中", maxTraceRequestContextBytes), Tools: []llm.ToolSpec{{Name: "Read", InputSchema: map[string]any{"type": "object"}}}}})
	events := store.Events("legacy")
	got := events[len(events)-1].RequestContext
	if got == nil || got.Turn != 8 || !got.Truncated || got.Limitation == "" || !utf8.ValidString(got.System) {
		t.Fatalf("incomplete snapshot not explicit: %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil || len(raw) > maxTraceRequestContextBytes {
		t.Fatalf("oversized snapshot=%d err=%v", len(raw), err)
	}
}

func TestTraceStreamingIdentityRespectsRequestToolAndOwner(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := NewTraceAdapter(store)
	a.SetSession("streams")
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, TraceStreamID: "request-1"})
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "first "})
	a.OnEvent(agent.Event{Kind: agent.EventInfo, Info: "diagnostic"})
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "second"})
	a.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Read", ToolUseID: "read"})
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "after tool"})
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, TraceStreamID: "request-retry"})
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "retry"})
	a.OnEvent(agent.Event{Kind: agent.EventTurnEnd})
	var text []session.TraceEvent
	for _, ev := range store.Events("streams") {
		if ev.Kind == "text" {
			text = append(text, ev)
		}
	}
	if len(text) != 4 || text[0].StreamID != "request-1" || text[1].StreamID != "request-1" || text[2].StreamID == "request-1" || text[2].StreamID == "" || text[3].StreamID != "request-retry" {
		t.Fatalf("response identities=%+v", text)
	}
}

func TestTraceStreamingLimitExplicitAndUnicodeSafe(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := NewTraceAdapter(store)
	a.SetSession("limit")
	a.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: strings.Repeat("中", maxTraceStreamBytes)})
	a.OnEvent(agent.Event{Kind: agent.EventTurnEnd})
	got := store.Events("limit")[0]
	if !got.Truncated || len(got.Text) > maxTraceStreamBytes || !utf8.ValidString(got.Text) || got.StreamID == "" {
		t.Fatalf("missing truncation metadata: bytes=%d truncated=%v", len(got.Text), got.Truncated)
	}
}
