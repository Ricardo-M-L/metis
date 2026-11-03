package webui

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestRequestTimingDoesNotChangeLegacyTraceMetrics(t *testing.T) {
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	original := []session.TracedNode{
		{Event: session.TraceEvent{Turn: 1, Kind: "text", TS: base, ElapsedMs: 1}},
		{Event: session.TraceEvent{Turn: 1, Kind: "tool_result", ToolName: "Bash", TS: base.Add(time.Second), ElapsedMs: 5000}},
		{Event: session.TraceEvent{Turn: 1, Kind: "tokens", TS: base.Add(10 * time.Second), Text: "input=1 output=10 cache_write=0 cache_read=0"}},
		{Event: session.TraceEvent{Turn: 1, Kind: "loop_done", TS: base.Add(10 * time.Second)}},
	}
	withTiming := append(append([]session.TracedNode(nil), original...),
		session.TracedNode{Event: session.TraceEvent{Turn: 1, Kind: "request_timing", TS: base.Add(-10 * time.Second)}},
		session.TracedNode{Event: session.TraceEvent{Turn: 1, Kind: "request_timing", TS: base.Add(30 * time.Second)}},
	)
	if a, b := traceTurnMetrics(original), traceTurnMetrics(withTiming); !reflect.DeepEqual(a, b) {
		t.Fatalf("metrics changed: before=%+v after=%+v", a, b)
	}
	if a, b := activeTraceDuration(original), activeTraceDuration(withTiming); a != b {
		t.Fatalf("duration changed: %d -> %d", a, b)
	}
	if a, b := activeTraceToolDuration(original), activeTraceToolDuration(withTiming); a != b {
		t.Fatalf("tool duration changed: %d -> %d", a, b)
	}
}

func TestRequestTimingTraceEndpointPreservesLegacyStatsAndReadability(t *testing.T) {
	old := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(old)
	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("trace unavailable")
	}
	store := rtpkg.CurrentTraceStore()
	defer store.Close()
	const sid = "timing-invariance"
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, ev := range []session.TraceEvent{
		{SessionID: sid, Turn: 1, Kind: "text", TS: base, Text: "answer"},
		{SessionID: sid, Turn: 1, Kind: "loop_done", TS: base.Add(10 * time.Second)},
	} {
		if err := store.Append(&ev); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := testServer(t)
	read := func() map[string]json.RawMessage {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId="+sid, nil))
		if rr.Code != 200 {
			t.Fatal(rr.Body.String())
		}
		var p map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	before := read()
	for _, at := range []time.Time{base.Add(-10 * time.Second), base.Add(30 * time.Second)} {
		if err := store.Append(&session.TraceEvent{SessionID: sid, Turn: 1, Kind: "request_timing", TS: at, Text: "pre_request_start"}); err != nil {
			t.Fatal(err)
		}
	}
	after := read()
	for _, key := range []string{"stats", "turnMetrics"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("%s changed: before=%s after=%s", key, before[key], after[key])
		}
	}
	var events []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(after["events"], &events); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Kind == "request_timing" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("diagnostic records no longer readable: %s", after["events"])
	}
}
