package webui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// The trajectory pane feeds off /api/trace: it must serve the session's
// event tree with depths and aggregate stats, and degrade cleanly when
// tracing is not installed.
func TestTraceEndpointServesTreeAndStats(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	oldStore := rtpkg.CurrentTraceStore()
	defer func() {
		rtpkg.SetTraceAdapter(oldAdapter)
		_ = oldStore
	}()

	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("InstallTrace returned nil adapter")
	}
	defer func() {
		if s := rtpkg.CurrentTraceStore(); s != nil {
			_ = s.Close()
		}
	}()
	adapter.SetSession("sess-trace-1")

	// Drive a small trajectory through the adapter the same way the
	// agent hook would: thinking, text, tool call, result, tokens.
	adapter.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "inspect "})
	adapter.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "state"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "let me check "})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "the build"})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolArgsDelta, ToolName: "Bash", ToolUseID: "t1", TextDelta: `{"command":"`})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolArgsDelta, ToolName: "Bash", ToolUseID: "t1", TextDelta: `echo hi"}`})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Bash", ToolUseID: "t1", ToolInput: map[string]any{"command": "echo hi"}})
	adapter.OnEvent(agent.Event{
		Kind:      agent.EventToolResult,
		ToolName:  "Bash",
		ToolUseID: "t1",
		Elapsed:   1500 * time.Millisecond,
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventTokens, InputTokens: 120, OutputTokens: 80})
	rtpkg.FlushTrace()

	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId=sess-trace-1", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Enabled     bool   `json:"enabled"`
		TotalEvents int    `json:"totalEvents"`
		HasMore     bool   `json:"hasMore"`
		NextCursor  string `json:"nextCursor"`
		Events      []struct {
			Kind      string `json:"kind"`
			Depth     int    `json:"depth"`
			Text      string `json:"text"`
			ToolUseID string `json:"toolUseID"`
		} `json:"events"`
		Stats struct {
			Turns        int   `json:"turns"`
			Steps        int   `json:"steps"`
			ToolCalls    int   `json:"toolCalls"`
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
			DurationMs   int64 `json:"durationMs"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !payload.Enabled {
		t.Fatal("trace endpoint should report enabled")
	}
	var kinds []string
	for _, ev := range payload.Events {
		kinds = append(kinds, ev.Kind)
	}
	want := []string{"thinking", "text", "tool_start", "tool_result", "tokens"}
	if len(kinds) != len(want) {
		t.Fatalf("want events %v, got %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("want events %v, got %v", want, kinds)
		}
	}
	// The tool_result nests under its tool_start (depth 1).
	if payload.Events[3].Depth != 1 {
		t.Fatalf("tool_result depth = %d, want 1", payload.Events[3].Depth)
	}
	if payload.Events[2].ToolUseID != "t1" || payload.Events[2].Text != `{"command":"echo hi"}` {
		t.Fatalf("tool row did not retain the stable id/full args: %+v", payload.Events[2])
	}
	// Reasoning and final text remain separate aggregated bursts.
	if payload.Events[0].Text != "inspect state" || payload.Events[1].Text != "let me check the build" {
		t.Fatalf("thinking/text bursts = %q / %q", payload.Events[0].Text, payload.Events[1].Text)
	}
	if payload.Stats.Steps != 3 {
		t.Fatalf("steps = %d, want thinking + text + tool", payload.Stats.Steps)
	}
	if payload.Stats.ToolCalls != 1 {
		t.Fatalf("toolCalls = %d", payload.Stats.ToolCalls)
	}
	if payload.Stats.InputTokens != 120 || payload.Stats.OutputTokens != 80 {
		t.Fatalf("token stats = %+v", payload.Stats)
	}
	if payload.TotalEvents != 5 || payload.HasMore || payload.NextCursor != "" {
		t.Fatalf("unexpected default page metadata: total=%d more=%v cursor=%q", payload.TotalEvents, payload.HasMore, payload.NextCursor)
	}

	// The newest page is returned first; its opaque cursor loads the older
	// half without overlap. Aggregate stats still describe the full trace.
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId=sess-trace-1&limit=3", nil))
	var newest struct {
		Events []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"events"`
		NextCursor string `json:"nextCursor"`
		HasMore    bool   `json:"hasMore"`
		Total      int    `json:"totalEvents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &newest); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || len(newest.Events) != 3 || !newest.HasMore || newest.NextCursor == "" || newest.Total != 5 {
		t.Fatalf("newest trace page = %d %+v", rr.Code, newest)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId=sess-trace-1&limit=3&cursor="+newest.NextCursor, nil))
	var older struct {
		Events []struct {
			ID string `json:"id"`
		} `json:"events"`
		HasMore bool `json:"hasMore"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &older); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || len(older.Events) != 2 || older.HasMore {
		t.Fatalf("older trace page = %d %+v", rr.Code, older)
	}
	ids := make(map[string]bool)
	for _, event := range newest.Events {
		ids[event.ID] = true
	}
	for _, event := range older.Events {
		if ids[event.ID] {
			t.Fatalf("trace cursor repeated event %q", event.ID)
		}
	}
}

func TestActiveTraceDurationExcludesIdleTimeBetweenTurns(t *testing.T) {
	start := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{Turn: 1, TS: start}},
		{Event: session.TraceEvent{Turn: 1, TS: start.Add(12 * time.Second)}},
		{Event: session.TraceEvent{Turn: 2, TS: start.Add(48 * time.Hour)}},
		{Event: session.TraceEvent{Turn: 2, TS: start.Add(48*time.Hour + 8*time.Second)}},
	}

	if got, want := activeTraceDuration(nodes), int64(20_000); got != want {
		t.Fatalf("activeTraceDuration() = %d ms, want %d ms", got, want)
	}
}

func TestTraceFromHistoryRestoresThinkingWithoutRedactedCiphertext(t *testing.T) {
	s, store := testServer(t)
	sid := store.NewSessionID()
	if err := store.WriteHeader(sid, "test-model", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sid, llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "question"},
			{Type: "thinking", Text: "forged user thinking"},
			{Type: "redacted_thinking", Data: "FORGED-USER-CIPHERTEXT"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const cipherText = "EuwBCkAG-HISTORY-CIPHERTEXT=="
	if err := store.AppendMessage(sid, llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "thinking", Text: "first summary\nmore detail"},
			{Type: "redacted_thinking", Data: cipherText},
			{Type: "text", Text: "final answer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	nodes := traceFromHistory(s, sid)
	wantKinds := []string{"user", "thinking", "thinking_redacted", "text"}
	if len(nodes) != len(wantKinds) {
		t.Fatalf("history trace = %+v, want kinds %v", nodes, wantKinds)
	}
	for i, node := range nodes {
		if node.Event.Kind != wantKinds[i] {
			t.Fatalf("node[%d].Kind = %q, want %q", i, node.Event.Kind, wantKinds[i])
		}
		if strings.Contains(node.Event.Text, cipherText) || strings.Contains(node.Event.Text, "forged") || strings.Contains(node.Event.Text, "FORGED") {
			t.Fatalf("node[%d] leaked provider ciphertext: %q", i, node.Event.Text)
		}
	}
	if got := nodes[1].Event.Text; got != "first summary\nmore detail" {
		t.Fatalf("thinking text = %q", got)
	}
	if got, want := nodes[2].Event.Text, "Reasoning redacted by provider"; got != want {
		t.Fatalf("redacted placeholder = %q, want %q", got, want)
	}
}

func TestRedactTraceTextProtectsStructuredCredentials(t *testing.T) {
	t.Parallel()
	input := `{"headers":{"Authorization":"Bearer top-secret","X-Trace":"keep"},"api_key":"hidden","nested":[{"password":"also-hidden"}],"command":"echo ok"}`
	got := redactTraceText(input)
	if strings.Contains(got, "top-secret") || strings.Contains(got, "hidden") || strings.Contains(got, "also-hidden") {
		t.Fatalf("credential leaked in %q", got)
	}
	for _, want := range []string{`"Authorization":"***redacted***"`, `"api_key":"***redacted***"`, `"password":"***redacted***"`, `"command":"echo ok"`, `"X-Trace":"keep"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("redacted trace %q missing %q", got, want)
		}
	}
	plain := "ordinary log token=descriptive, not structured JSON"
	if got := redactTraceText(plain); got != plain {
		t.Fatalf("plain trace changed: %q", got)
	}
}

func TestCoalesceTraceToolArgsPreservesInterruptedStreams(t *testing.T) {
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{Kind: "tool_args", ToolName: "Read", ToolUseID: "a", Text: `{"path":"`}},
		{Event: session.TraceEvent{Kind: "tool_args", ToolName: "Read", ToolUseID: "b", Text: `{"path":"b`}},
		{Event: session.TraceEvent{Kind: "tool_args", ToolName: "Read", ToolUseID: "a", Text: `a"}`}},
		{Event: session.TraceEvent{Kind: "tool_start", ToolName: "Read", ToolUseID: "a", Text: `{"path":"authoritative"}`}},
	}
	got := coalesceTraceToolArgs(nodes)
	if len(got) != 2 {
		t.Fatalf("coalesced rows = %d, want 2: %+v", len(got), got)
	}
	if got[0].Event.Kind != "tool_start" || got[0].Event.ToolUseID != "a" || got[0].Event.Text != `{"path":"authoritative"}` {
		t.Fatalf("completed stream = %+v", got[0].Event)
	}
	if got[1].Event.Kind != "tool_start" || got[1].Event.ToolUseID != "b" || got[1].Event.Text != `{"path":"b` {
		t.Fatalf("interrupted stream = %+v", got[1].Event)
	}
}

func TestTraceEndpointRejectsBadSessionID(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId=../escape", nil))
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestTraceEndpointRejectsInvalidPagination(t *testing.T) {
	s, _ := testServer(t)
	for _, rawURL := range []string{
		"/api/trace?sessionId=valid&limit=0",
		"/api/trace?sessionId=valid&limit=2001",
		"/api/trace?sessionId=valid&cursor=not-base64!",
	} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", rawURL, nil))
		if rr.Code != 400 {
			t.Fatalf("%s status = %d, want 400: %s", rawURL, rr.Code, rr.Body.String())
		}
	}
}
