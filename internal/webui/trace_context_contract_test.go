package webui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceContextContractEveryPageAndStructuredTiming(t *testing.T) {
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	a := rtpkg.InstallTrace(t.TempDir())
	if a == nil {
		t.Fatal("trace unavailable")
	}
	defer rtpkg.CurrentTraceStore().Close()
	a.SetSession("contract")
	a.OnEvent(agent.Event{Kind: agent.EventRequestContext, TraceStreamID: "response-id", RequestContext: &agent.TraceRequestContext{System: "actual assembled system", Tools: []llm.ToolSpec{{Name: "Read", InputSchema: map[string]any{"type": "object"}}}}})
	want := "  " + strings.Repeat("complete text ", 300) + "\n"
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: want})
	a.OnEvent(agent.Event{Kind: agent.EventTurnEnd})
	now := time.Now()
	if err := rtpkg.CurrentTraceStore().Append(&session.TraceEvent{SessionID: "contract", Turn: 1, Kind: "request_timing", RequestTiming: &transport.TimingRecord{RequestID: "timing-id", Phase: "http_do_start", At: now, ElapsedMS: 0}}); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	var cursor string
	for page := 0; page < 3; page++ {
		path := "/api/trace?sessionId=contract&limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		var payload struct {
			InitialContext session.TraceRequestContext `json:"initialContext"`
			Events         []traceEventView            `json:"events"`
			NextCursor     string                      `json:"nextCursor"`
			Total          int                         `json:"totalEvents"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &payload) != nil {
			t.Fatalf("trace response=%d %s", rr.Code, rr.Body.String())
		}
		if !payload.InitialContext.Available || payload.InitialContext.System != "actual assembled system" || len(payload.InitialContext.Tools) != 1 || payload.Total != 3 {
			t.Fatalf("page %d missing true initial snapshot: %+v", page, payload)
		}
		if len(payload.Events) != 1 {
			t.Fatalf("page size=%d", len(payload.Events))
		}
		ev := payload.Events[0]
		if ev.Kind == "request_timing" && (ev.RequestTiming == nil || ev.RequestTiming.RequestID != "timing-id" || !strings.Contains(rr.Body.String(), `"elapsed_ms":0`)) {
			t.Fatal("structured zero timing lost")
		}
		if ev.Kind == "text" && (ev.Text != want || ev.StreamID != "response-id" || ev.SessionID != "contract" || ev.Sequence != 2) {
			t.Fatalf("original full text/identity/sequence changed: %+v", ev)
		}
		cursor = payload.NextCursor
	}
}

func TestTraceContextUnavailableDoesNotClaimSavedHeader(t *testing.T) {
	_, initial := splitInitialTraceContext([]session.TracedNode{{Event: session.TraceEvent{Kind: "user", Text: "legacy question"}}})
	if initial.Available || initial.System != "" || initial.Limitation == "" {
		t.Fatalf("missing snapshot should be explicit: %+v", initial)
	}
}

func TestTraceContextUsesExplicitStoreBinding(t *testing.T) {
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	global := rtpkg.InstallTrace(t.TempDir())
	if global == nil {
		t.Fatal("trace unavailable")
	}
	defer rtpkg.CurrentTraceStore().Close()
	global.SetSession("same-id")
	global.OnEvent(agent.Event{Kind: agent.EventRequestContext, RequestContext: &agent.TraceRequestContext{System: "unrelated global system"}})
	bound, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if err := bound.Append(&session.TraceEvent{SessionID: "same-id", Kind: "request_context", RequestContext: &session.TraceRequestContext{Available: true, Source: "logical-request", System: "explicit bound system"}}); err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", nil, sessionStore, RuntimeBindings{TraceStore: bound})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId=same-id", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "explicit bound system") || strings.Contains(rr.Body.String(), "unrelated global system") {
		t.Fatalf("explicit trace store ignored: %d %s", rr.Code, rr.Body.String())
	}
}
