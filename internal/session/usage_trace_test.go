package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadUsageEventsOnlyCompleteDistinctSessionRows(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	path := filepath.Join(dir, "session.jsonl")
	data := "{\"id\":\"a\",\"session_id\":\"session\",\"turn\":1,\"kind\":\"tokens\",\"usage_model\":\"model-a\",\"text\":\"input=5 output=2 cache_write=0 cache_read=0\"}\n" +
		"{\"id\":\"a\",\"session_id\":\"session\",\"turn\":1,\"kind\":\"tokens\",\"usage_model\":\"model-a\"}\n" +
		"{\"id\":\"b\",\"session_id\":\"session\",\"turn\":1,\"kind\":\"tokens\",\"text\":\"input=3 output=1 cache_write=0 cache_read=0\"}\n" +
		"{\"id\":\"c\",\"session_id\":\"elsewhere\",\"turn\":1,\"kind\":\"tokens\"}\n" +
		"{\"id\":\"d\",\"session_id\":\"session\",\"turn\":1,\"kind\":\"text\",\"text\":\"private content\"}\n" +
		"not json\n" +
		"{\"id\":\"incomplete\",\"session_id\":\"session\",\"turn\":1,\"kind\":\"tokens\"}"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadUsageEvents("session")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UsageModel != "model-a" || got[1].UsageModel != "" {
		t.Fatalf("got %+v", got)
	}
	if _, err := store.ReadUsageEvents("../escape"); err == nil {
		t.Fatal("unsafe session ID accepted")
	}
	if got, err := store.ReadUsageEvents("missing"); err != nil || len(got) != 0 {
		t.Fatalf("missing: %+v %v", got, err)
	}
	if len(store.loaded) != 0 || len(store.events) != 0 {
		t.Fatal("usage scan unexpectedly populated trace index")
	}
}

func TestReadUsageEventsFlushesActiveWriter(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Append(&TraceEvent{SessionID: "active", Turn: 1, Kind: "tokens", UsageModel: "actual-model", Text: "input=1 output=2 cache_write=0 cache_read=0"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadUsageEvents("active")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UsageModel != "actual-model" {
		t.Fatalf("got %+v", got)
	}
}
