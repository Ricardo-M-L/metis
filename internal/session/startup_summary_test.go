package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/turnprogress"
)

func TestStartupSummaryPersistsAndSurvivesUsageReconciliation(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	zero, first, total := int64(0), int64(7), int64(15)
	start := time.Now()
	metric := MessageMetric{Turn: 1, StartedAt: start, CompletedAt: start.Add(15 * time.Millisecond), DurationMS: 15,
		StartupSummary: &StartupSummary{RequestID: "parent-request", Source: turnprogress.SourceServerObserved, ReceivedMS: &zero, FirstThinkingMS: &first, TotalMS: &total}}
	if _, err = store.ReconcileMessageMetric("summary", metric); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReconcileMessageMetric("summary", MessageMetric{Turn: 1, OutputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if err = store.ReconcileTraceUsageSnapshot("summary", map[int]CostSnapshot{1: {OutputTokens: 12}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadMessageMetrics("summary")
	if err != nil || len(got) != 1 || got[0].StartupSummary == nil || got[0].OutputTokens != 12 {
		t.Fatalf("summary after partial usage: %+v, %v", got, err)
	}
	raw, err := json.Marshal(got[0])
	if err != nil || !strings.Contains(string(raw), `"receivedMs":0`) || !strings.Contains(string(raw), `"firstByteMs":null`) || !strings.Contains(string(raw), `"firstThinkingMs":7`) {
		t.Fatalf("observed zero/unknown lost: %s, %v", raw, err)
	}
}

func TestStartupSummaryLegacyAndPartialMerge(t *testing.T) {
	var legacy MessageMetric
	if err := json.Unmarshal([]byte(`{"turn":1,"duration_ms":15}`), &legacy); err != nil || legacy.StartupSummary != nil {
		t.Fatalf("legacy summary should remain unknown: %+v, %v", legacy, err)
	}
	zero, first, total := int64(0), int64(4), int64(12)
	current := MessageMetric{Turn: 1, StartupSummary: &StartupSummary{RequestID: "r", ReceivedMS: &zero, FirstThinkingMS: &first}}
	merged := mergeMessageMetric(current, MessageMetric{Turn: 1, StartupSummary: &StartupSummary{RequestID: "r", TotalMS: &total}})
	if merged.StartupSummary == current.StartupSummary || merged.StartupSummary.FirstThinkingMS == current.StartupSummary.FirstThinkingMS || *merged.StartupSummary.FirstThinkingMS != 4 || *merged.StartupSummary.TotalMS != 12 {
		t.Fatalf("partial summary merge: %+v", merged.StartupSummary)
	}
}
