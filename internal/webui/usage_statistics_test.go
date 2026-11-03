package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestUsageStatisticsAggregatesLedgersWithoutInventingDailyHistory(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "legacy", "metric-only", "inconsistent", "no-usage"} {
		if err := store.WriteHeader(id, "model", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WriteCost("one", session.CostSnapshot{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("legacy", session.CostSnapshot{InputTokens: 200}); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive("legacy"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("inconsistent", session.CostSnapshot{InputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("test+8", 8*60*60)
	started := time.Date(2026, 10, 29, 15, 55, 0, 0, time.UTC)
	finished := time.Date(2026, 10, 29, 16, 5, 0, 0, time.UTC) // Oct 30 local
	for _, m := range []session.MessageMetric{
		{Turn: 1, StartedAt: started, CompletedAt: finished, DurationMS: 1000, InputTokens: 20, OutputTokens: 5, CacheReadTokens: 3},
		{Turn: 1, StartedAt: started, CompletedAt: finished, DurationMS: 2000, InputTokens: 40, OutputTokens: 7, CacheReadTokens: 3},
		{Turn: 2, StartedAt: started, CompletedAt: finished.Add(24 * time.Hour), DurationMS: 3000, InputTokens: 10},
	} {
		if err := store.AppendMessageMetric("one", m); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendMessageMetric("metric-only", session.MessageMetric{Turn: 1, StartedAt: started, CompletedAt: finished, DurationMS: 4000, OutputTokens: 9}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessageMetric("inconsistent", session.MessageMetric{Turn: 1, StartedAt: started, CompletedAt: finished, InputTokens: 8}); err != nil {
		t.Fatal(err)
	}
	child := session.Header{ID: "child", CreatedAt: started, Model: "model", SubAgentOf: "one"}
	if err := store.WriteHeaderFull(child); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("child", session.CostSnapshot{InputTokens: 999}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "one.message-metrics.jsonl"), append(mustRead(t, filepath.Join(store.Dir, "one.message-metrics.jsonl")), []byte(`{"turn":3,"input_tokens":2}`+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := collectUsageStatistics(store, time.Date(2026, 10, 31, 12, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Coverage.Sessions != 5 || got.Coverage.SessionsWithUsage != 3 || got.Coverage.SessionsWithoutUsage != 2 || got.Coverage.MetricOnlySessions != 1 {
		t.Fatalf("coverage: %+v", got.Coverage)
	}
	if got.Total.TotalTokens != 344 || got.Total.InputTokens != 305 || got.Total.OutputTokens != 29 || got.Total.CacheReadTokens != 10 {
		t.Fatalf("total: %+v", got.Total)
	}
	if got.Coverage.UnattributedTokens != 270 || got.Coverage.InconsistentSessions != 1 || got.Coverage.InvalidMetricTurns != 1 {
		t.Fatalf("partial coverage: %+v", got.Coverage)
	}
	if len(got.Daily) != 2 || got.Daily[0].Date != "2026-10-30" || got.Daily[0].TotalTokens != 67 || got.Daily[0].Turns != 3 || got.Daily[1].TotalTokens != 10 {
		t.Fatalf("daily: %+v", got.Daily)
	}
	if got.LongestTurnMs != 4000 || got.LongestSessionMs != 5000 || got.Coverage.SessionsWithDuration != 2 || got.CurrentStreak != 2 || got.LongestStreak != 2 || got.PeakDayTokens != 67 || got.ModelBreakdownAvailable {
		t.Fatalf("summary: %+v", got)
	}
}

func TestUsageStatisticsLongestSessionUsesOnlyRecordedCompletedTurns(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"multi", "single", "unknown"} {
		if err := store.WriteHeader(id, "model", ""); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		id     string
		metric session.MessageMetric
	}{
		{"multi", session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), DurationMS: 6000}},
		{"multi", session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), DurationMS: 8000}},
		{"multi", session.MessageMetric{Turn: 2, StartedAt: start, CompletedAt: start.Add(time.Second), DurationMS: 4000}},
		{"single", session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), DurationMS: 10000}},
		{"single", session.MessageMetric{Turn: 2, StartedAt: start, CompletedAt: start.Add(time.Second), DurationMS: -20}},
	} {
		if err := store.AppendMessageMetric(item.id, item.metric); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(store.Dir, "multi.message-metrics.jsonl")
	rows := append(mustRead(t, path), []byte("not-json\n{\"turn\":3,\"duration_ms\":999999}\n")...)
	if err := os.WriteFile(path, rows, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := collectUsageStatistics(store, start, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got.LongestSessionMs != 12000 || got.LongestTurnMs != 10000 || got.Coverage.SessionsWithDuration != 2 || got.Coverage.InvalidMetricTurns != 1 {
		t.Fatalf("duration summary: %+v coverage: %+v", got, got.Coverage)
	}
}

func TestUsageStatisticsNoRecordedDurationRemainsUnavailable(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("old", "model", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("old", session.CostSnapshot{InputTokens: 50}); err != nil {
		t.Fatal(err)
	}
	got, err := collectUsageStatistics(store, time.Now(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got.LongestSessionMs != 0 || got.Coverage.SessionsWithDuration != 0 {
		t.Fatalf("invented duration: %+v", got)
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUsageStatisticsHTTPGuardAndEmptyState(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", nil, store)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/usage/statistics", nil)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, request)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	var body usageStatistics
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Scope != "local" || body.Daily == nil || body.Coverage.Sessions != 0 || body.ModelBreakdownAvailable {
		t.Fatalf("body: %+v", body)
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/usage/statistics", nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	s.handler().ServeHTTP(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site status %d", w.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/usage/statistics", nil)
	w = httptest.NewRecorder()
	s.handler().ServeHTTP(w, request)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", w.Code)
	}
}

func TestUsageStatisticsModelsAreAttributedOnlyFromKnownTraceWithinLedger(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trace, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trace.Close() })
	for _, id := range []string{"known", "old", "overflow"} {
		if err := store.WriteHeader(id, "unrelated-header-model", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WriteCost("known", session.CostSnapshot{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("old", session.CostSnapshot{InputTokens: 50}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("overflow", session.CostSnapshot{InputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		id     string
		metric session.MessageMetric
	}{
		{"known", session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), InputTokens: 30, OutputTokens: 5, CacheReadTokens: 2}},
		{"known", session.MessageMetric{Turn: 2, StartedAt: start, CompletedAt: start.Add(time.Second), InputTokens: 10, OutputTokens: 2}},
		{"overflow", session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), InputTokens: 4}},
	} {
		if err := store.AppendMessageMetric(item.id, item.metric); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []*session.TraceEvent{
		{ID: "a", SessionID: "known", Turn: 1, Kind: "tokens", Text: "input=30 output=5 cache_write=0 cache_read=2", UsageModel: "model-a"},
		{ID: "a", SessionID: "known", Turn: 1, Kind: "tokens", Text: "input=30 output=5 cache_write=0 cache_read=2", UsageModel: "model-a"},
		{ID: "b", SessionID: "known", Turn: 2, Kind: "tokens", Text: "input=10 output=2 cache_write=0 cache_read=0", UsageModel: "model-b"},
		{ID: "old", SessionID: "old", Turn: 1, Kind: "tokens", Text: "input=30 output=0 cache_write=0 cache_read=0"},
		{ID: "overflow", SessionID: "overflow", Turn: 1, Kind: "tokens", Text: "input=4 output=0 cache_write=0 cache_read=0", UsageModel: "model-c"},
	} {
		if err := trace.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := collectUsageStatistics(store, time.Now(), time.UTC, trace)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ModelBreakdownAvailable || len(got.Models) != 2 || got.Models[0].Model != "model-a" || got.Models[0].TotalTokens != 37 || got.Models[1].Model != "model-b" || got.Models[1].TotalTokens != 12 {
		t.Fatalf("models: %+v", got.Models)
	}
	if got.Total.TotalTokens != 178 || got.Coverage.UnattributedModelTokens != 129 || got.Coverage.ModelInconsistentSessions != 1 {
		t.Fatalf("coverage: %+v total=%+v", got.Coverage, got.Total)
	}
}

func TestUsageStatisticsDoesNotAttributeUncommittedTraceToOldLedger(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trace, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trace.Close() })
	if err := store.WriteHeader("s", "header-model", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCost("s", session.CostSnapshot{InputTokens: 100}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if err := store.AppendMessageMetric("s", session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), InputTokens: 100}); err != nil {
		t.Fatal(err)
	}
	// A new trace appears before the durable observer records turn 2. It must
	// not be paid for out of the old turn's 100 unattributed tokens.
	if err := trace.Append(&session.TraceEvent{ID: "new", SessionID: "s", Turn: 2, Kind: "tokens", UsageModel: "new-model", Text: "input=10 output=0 cache_write=0 cache_read=0"}); err != nil {
		t.Fatal(err)
	}
	got, err := collectUsageStatistics(store, start, time.UTC, trace)
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelBreakdownAvailable || len(got.Models) != 0 || got.Coverage.UnattributedModelTokens != 100 {
		t.Fatalf("uncommitted event attributed: %+v", got)
	}
	// Once the ledger and per-turn metric have both committed, it is safe.
	if _, err := store.ReconcileMessageMetric("s", session.MessageMetric{Turn: 2, StartedAt: start, CompletedAt: start.Add(time.Second), InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	got, err = collectUsageStatistics(store, start, time.UTC, trace)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "new-model" || got.Models[0].TotalTokens != 10 || got.Coverage.UnattributedModelTokens != 100 {
		t.Fatalf("committed event: %+v", got)
	}
}

func TestUsageStatisticsDoesNotAttributeTraceBeyondTurnMetricOrOverflow(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trace, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trace.Close() })
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"s", "overflow"} {
		if err := store.WriteHeader(id, "header-model", ""); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteCost(id, session.CostSnapshot{InputTokens: 100}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessageMetric(id, session.MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(time.Second), InputTokens: 100}); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []*session.TraceEvent{
		{ID: "unknown", SessionID: "s", Turn: 1, Kind: "tokens", Text: "input=100 output=0 cache_write=0 cache_read=0"},
		{ID: "new", SessionID: "s", Turn: 1, Kind: "tokens", UsageModel: "new-model", Text: "input=10 output=0 cache_write=0 cache_read=0"},
		{ID: "max", SessionID: "overflow", Turn: 1, Kind: "tokens", UsageModel: "bad-model", Text: "input=9223372036854775807 output=0 cache_write=0 cache_read=0"},
		{ID: "one", SessionID: "overflow", Turn: 1, Kind: "tokens", UsageModel: "bad-model", Text: "input=1 output=0 cache_write=0 cache_read=0"},
	} {
		if err := trace.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := collectUsageStatistics(store, start, time.UTC, trace)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 0 || got.Coverage.UnattributedModelTokens != 200 || got.Coverage.ModelInconsistentSessions != 2 {
		t.Fatalf("inconsistent traces attributed: %+v", got)
	}
}
