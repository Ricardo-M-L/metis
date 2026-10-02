package webui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// A fresh Desktop session is queried before its private worker writes any
// events. The same parent store must subsequently observe that worker's file,
// without a restart or borrowing token counts from another conversation.
func TestDesktopTraceRefreshesIsolatedWorkerUsage(t *testing.T) {
	old := rtpkg.CurrentTraceAdapter()
	dir := t.TempDir()
	if rtpkg.InstallTrace(dir) == nil {
		t.Fatal("trace unavailable")
	}
	t.Cleanup(func() { _ = rtpkg.CurrentTraceStore().Close(); rtpkg.SetTraceAdapter(old) })
	s, _ := testServer(t)
	s.traceStore = rtpkg.CurrentTraceStore()
	const sid = "isolated-footer-usage"
	read := func() traceStats {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId="+sid, nil))
		if rr.Code != 200 {
			t.Fatalf("trace %d: %s", rr.Code, rr.Body.String())
		}
		var body struct {
			Stats traceStats `json:"stats"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Stats
	}
	if before := read(); before.InputTokens != 0 {
		t.Fatal(before)
	}
	worker, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	start := time.Now().Add(-2 * time.Second)
	for _, event := range []session.TraceEvent{
		{Kind: "user", Text: "footer verification", TS: start},
		{Kind: "text", Text: "done", TS: start.Add(time.Second)},
		{Kind: "tokens", Text: "input=64 output=16 cache_write=0 cache_read=64", TS: start.Add(2 * time.Second)},
		{Kind: "loop_done", Text: "end_turn", TS: start.Add(2 * time.Second)},
	} {
		event.SessionID, event.Turn = sid, 1
		if err := worker.Append(&event); err != nil {
			t.Fatal(err)
		}
	}
	if err := worker.SyncSession(sid); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got := read()
		if got.Turns != 1 || got.Steps != 2 || got.InputTokens != 128 || got.OutputTokens != 16 || got.CacheRead != 64 || got.CacheHitRate != 50 || got.TokPerSec != 8 {
			t.Fatalf("refresh %d lost/doubled worker stats: %+v", i, got)
		}
	}
	// The completion repair also keeps a durable cost ledger. Reconciliation
	// must be idempotent even after the API has already imported the trace.
	for i := 0; i < 2; i++ {
		s.reconcilePersistedTraceUsage(sid)
	}
	cost, ok, err := s.store.ReadCost(sid)
	if err != nil || !ok || cost.InputTokens != 64 || cost.OutputTokens != 16 || cost.CacheReadTokens != 64 {
		t.Fatalf("worker cost not reconciled: %+v, %v, %v", cost, ok, err)
	}
}
