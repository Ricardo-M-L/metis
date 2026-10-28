package webui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceRecoveredToolStaysBeforeLaterTurn(t *testing.T) {
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{Sequence: 1359, Turn: 30, Kind: "tool_args", ToolName: "ToolSearch", ToolUseID: "search", TraceCallID: "call-old", Text: `{"query":"select:Read"}`}},
		{Event: session.TraceEvent{Sequence: 1361, Turn: 30, Kind: "tool_result", ToolName: "ToolSearch", ToolUseID: "search", TraceCallID: "call-old", Text: "loaded"}},
		{Event: session.TraceEvent{Sequence: 1852, Turn: 49, Kind: "user", Text: "new question"}},
		{Event: session.TraceEvent{Sequence: 1858, Turn: 49, Kind: "text", Text: "new answer"}},
	}
	got := coalesceTraceToolArgs(nodes)
	if len(got) != len(nodes) {
		t.Fatalf("rows = %d, want %d", len(got), len(nodes))
	}
	for i, want := range []int64{1359, 1361, 1852, 1858} {
		if got[i].Event.Sequence != want {
			t.Fatalf("row[%d] sequence = %d, want %d; old tool must not follow Turn 49", i, got[i].Event.Sequence, want)
		}
	}
	if got[0].Event.Kind != "tool_start" || got[1].Event.Kind != "tool_result" {
		t.Fatalf("recovered call = %+v", got[:2])
	}
	if nodes[0].Event.Kind != "tool_args" {
		t.Fatal("projection changed persisted evidence")
	}
}

func TestTracePartialArgumentsCannotCrossCallOwnership(t *testing.T) {
	for _, test := range []struct {
		name          string
		first, second session.TraceEvent
	}{
		{name: "turn", first: session.TraceEvent{Turn: 30}, second: session.TraceEvent{Turn: 49}},
		{name: "invocation", first: session.TraceEvent{Turn: 1, TraceInvocationID: "parent"}, second: session.TraceEvent{Turn: 1, TraceInvocationID: "child"}},
		{name: "call", first: session.TraceEvent{Turn: 1, TraceInvocationID: "same", TraceCallID: "first"}, second: session.TraceEvent{Turn: 1, TraceInvocationID: "same", TraceCallID: "second"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			first, second := test.first, test.second
			first.Kind, second.Kind = "tool_args", "tool_args"
			first.ToolName, second.ToolName = "Read", "Read"
			first.ToolUseID, second.ToolUseID = "reused", "reused"
			first.Text, second.Text = `{"path":"first"}`, `{"path":"second"}`
			got := coalesceTraceToolArgs([]session.TracedNode{{Event: first}, {Event: second}})
			if len(got) != 2 || got[0].Event.Text != first.Text || got[1].Event.Text != second.Text {
				t.Fatalf("independent calls merged: %+v", got)
			}
		})
	}
}

func TestTraceResultSealsRecoveredArgumentsBeforeProviderIDReuse(t *testing.T) {
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{Turn: 1, Kind: "tool_args", ToolName: "Read", ToolUseID: "same", Text: "first"}},
		{Event: session.TraceEvent{Turn: 1, Kind: "tool_result", ToolName: "Read", ToolUseID: "same", Text: "first result"}},
		{Event: session.TraceEvent{Turn: 1, Kind: "tool_args", ToolName: "Read", ToolUseID: "same", Text: "second"}},
		{Event: session.TraceEvent{Turn: 1, Kind: "tool_result", ToolName: "Read", ToolUseID: "same", Text: "second result"}},
	}
	got := coalesceTraceToolArgs(nodes)
	if len(got) != 4 || got[0].Event.Text != "first" || got[2].Event.Text != "second" {
		t.Fatalf("completed argument streams merged across occurrences: %+v", got)
	}
}

func TestTraceRecoveryLegacyIdentityAndAmbiguity(t *testing.T) {
	t.Run("anonymous legacy fragments inherit the unique result owner", func(t *testing.T) {
		got := coalesceTraceToolArgs([]session.TracedNode{
			{Event: session.TraceEvent{Turn: 1, Kind: "tool_args", Text: "{}"}},
			{Event: session.TraceEvent{Turn: 1, Kind: "tool_result", ToolName: "Read", ToolUseID: "read", TraceInvocationID: "owner", TraceCallID: "call", Text: "done"}},
		})
		if len(got) != 2 || got[0].Event.TraceCallID != "call" || got[0].Event.TraceInvocationID != "owner" || got[0].Event.ToolUseID != "read" || got[0].Event.ToolName != "Read" {
			t.Fatalf("recovered owner = %+v", got)
		}
	})
	t.Run("a name-only result cannot choose among concurrent calls", func(t *testing.T) {
		got := coalesceTraceToolArgs([]session.TracedNode{
			{Event: session.TraceEvent{Turn: 1, Kind: "tool_args", ToolName: "Read", ToolUseID: "a", Text: "first"}},
			{Event: session.TraceEvent{Turn: 1, Kind: "tool_args", ToolName: "Read", ToolUseID: "b", Text: "second"}},
			{Event: session.TraceEvent{Turn: 1, Kind: "tool_result", ToolName: "Read", TraceCallID: "unknown", Text: "unattributed"}},
		})
		if len(got) != 3 || got[0].Event.TraceCallID != "" || got[1].Event.TraceCallID != "" || got[2].Event.Kind != "tool_result" {
			t.Fatalf("ambiguous result was assigned to a call: %+v", got)
		}
	})
	t.Run("same-name start cannot consume an older turn's fragments", func(t *testing.T) {
		got := coalesceTraceToolArgs([]session.TracedNode{
			{Event: session.TraceEvent{Turn: 30, Kind: "tool_args", ToolName: "Read", Text: "old arguments"}},
			{Event: session.TraceEvent{Turn: 49, Kind: "tool_start", ToolName: "Read", Text: "{}"}},
		})
		if len(got) != 2 || got[0].Event.Turn != 30 || got[1].Event.Turn != 49 || got[1].Event.Text != "{}" {
			t.Fatalf("new start consumed old arguments: %+v", got)
		}
	})
}

func TestTraceEndpointPreservesToolCallIdentity(t *testing.T) {
	old := rtpkg.CurrentTraceAdapter()
	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("trace adapter unavailable")
	}
	t.Cleanup(func() { _ = rtpkg.CurrentTraceStore().Close(); rtpkg.SetTraceAdapter(old) })
	if err := rtpkg.CurrentTraceStore().Append(&session.TraceEvent{
		SessionID: "tool-identity", Turn: 1, Kind: "tool_start", ToolName: "Read", ToolUseID: "provider-id",
		TraceInvocationID: "invocation", TraceCallID: "occurrence",
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId=tool-identity", nil))
	var payload struct {
		Events []struct {
			Invocation string `json:"traceInvocationID"`
			Call       string `json:"traceCallID"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || len(payload.Events) != 1 || payload.Events[0].Invocation != "invocation" || payload.Events[0].Call != "occurrence" {
		t.Fatalf("trace API dropped call ownership: status=%d events=%+v", rr.Code, payload.Events)
	}
}
