package session

import (
	"testing"
	"time"
)

func TestTraceLatestInputBoundaryUsesRootTurnAndFirstInput(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sid = "input-boundary"
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	appendEvent := func(ev TraceEvent) {
		t.Helper()
		ev.SessionID = sid
		if err := store.Append(&ev); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(TraceEvent{Turn: 1, Kind: "user", TS: at})
	appendEvent(TraceEvent{Turn: 2, Kind: "context", TS: at.Add(time.Minute)})
	appendEvent(TraceEvent{Turn: 2, Kind: "context", Source: "peer", TS: at.Add(2 * time.Minute)})
	if turn, start := store.LatestInputBoundary(sid); turn != 2 || !start.Equal(at.Add(time.Minute)) {
		t.Fatalf("same-turn context moved automatic input: turn=%d at=%s", turn, start)
	}
	appendEvent(TraceEvent{Turn: 2, Kind: "user", TS: at.Add(3 * time.Minute)})
	appendEvent(TraceEvent{Turn: 2, Kind: "user", TS: at.Add(4 * time.Minute)})
	appendEvent(TraceEvent{Turn: 9, Kind: "user", SubAgentOf: "child", TS: at.Add(5 * time.Minute)})
	appendEvent(TraceEvent{Turn: 10, Kind: "context", ParentID: "child", TS: at.Add(6 * time.Minute)})
	appendEvent(TraceEvent{Turn: 11, Kind: "context", TraceParentInvocationID: "parent", TS: at.Add(7 * time.Minute)})
	appendEvent(TraceEvent{Turn: 1, Kind: "user", TS: at.Add(8 * time.Minute)})
	assertBoundary := func(target *TraceStore) {
		t.Helper()
		if turn, start := target.LatestInputBoundary(sid); turn != 2 || !start.Equal(at.Add(3*time.Minute)) {
			t.Fatalf("latest root input = turn %d at %s", turn, start)
		}
		for _, invalid := range []string{"", "..", "../other", "unknown"} {
			if turn, start := target.LatestInputBoundary(invalid); turn != 0 || !start.IsZero() {
				t.Fatalf("missing/invalid input %q = turn %d at %s", invalid, turn, start)
			}
		}
	}
	assertBoundary(store)
	if err := store.Sync(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	assertBoundary(reloaded)
}

func TestTraceLatestInputBoundaryDeleteAllowsFreshSession(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sid = "reused-input-owner"
	old := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := store.Append(&TraceEvent{SessionID: sid, Turn: 9, Kind: "user", TS: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(sid); err != nil {
		t.Fatal(err)
	}
	if turn, at := store.LatestInputBoundary(sid); turn != 0 || !at.IsZero() {
		t.Fatalf("deleted session kept input boundary: turn=%d at=%s", turn, at)
	}
	fresh := old.Add(time.Hour)
	if err := store.Append(&TraceEvent{SessionID: sid, Turn: 1, Kind: "user", TS: fresh}); err != nil {
		t.Fatal(err)
	}
	if turn, at := store.LatestInputBoundary(sid); turn != 1 || !at.Equal(fresh) {
		t.Fatalf("old boundary blocked reused session: turn=%d at=%s", turn, at)
	}
}

func TestTraceFlushSessionPublishesInputToAnotherReader(t *testing.T) {
	dir := t.TempDir()
	writer, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	const sid = "flush-input"
	at := time.Now()
	if err := writer.Append(&TraceEvent{SessionID: sid, Turn: 1, Kind: "user", TS: at}); err != nil {
		t.Fatal(err)
	}
	if err := writer.FlushSession(sid); err != nil {
		t.Fatal(err)
	}
	if err := reader.Refresh(sid); err != nil {
		t.Fatal(err)
	}
	if turn, start := reader.LatestInputBoundary(sid); turn != 1 || !start.Equal(at) {
		t.Fatalf("published input unavailable to independent reader: turn=%d at=%s", turn, start)
	}
	if err := writer.FlushSession("../invalid"); err == nil {
		t.Fatal("invalid trace owner accepted")
	}
	if err := writer.FlushSession("missing"); err != nil {
		t.Fatalf("empty session flush = %v", err)
	}
}
