package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestSessionMetricsOptions(t *testing.T) {
	for _, args := range [][]string{{"fixture", "--json"}, {"--json", "fixture"}} {
		got, err := parseSessionMetricsOptions(args)
		if err != nil || got.id != "fixture" || !got.jsonOutput {
			t.Fatalf("parseSessionMetricsOptions(%q) = %#v, %v", args, got, err)
		}
	}
	for _, args := range [][]string{nil, {"--json"}, {"a", "b"}, {"--unknown", "a"}, {"../a"}, {"a/b"}, {""}, {"."}, {".."}} {
		if _, err := parseSessionMetricsOptions(args); err == nil {
			t.Errorf("parseSessionMetricsOptions(%q) unexpectedly succeeded", args)
		}
	}
}

func TestPrintSessionMetricsJSONReadsSidecarWithoutWriting(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC)
	zero := int64(0)
	want := session.MessageMetric{Turn: 1, StartedAt: started, CompletedAt: started.Add(time.Second), DurationMS: 1000, TTFTMS: 200, InputTokens: 2, OutputTokens: 3,
		StartupSummary: &session.StartupSummary{RequestID: "request", Source: "server_observed", ReceivedMS: &zero, QueueMS: &zero}}
	if err := store.AppendMessageMetric("fixture", want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "fixture.message-metrics.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printSessionMetrics(store, []string{"fixture", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var got []session.MessageMetric
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []session.MessageMetric{want}) {
		t.Fatalf("metrics = %#v, want %#v", got, want)
	}
	if !strings.Contains(out.String(), `"receivedMs":0`) || !strings.Contains(out.String(), `"mcpMs":null`) {
		t.Fatalf("JSON lost measured zero or unknown startup milestone: %s", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("metrics query changed sidecar: %q -> %q (%v)", before, after, err)
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("metrics query created files: %v (%v)", entries, err)
	}
}

func TestPrintSessionMetricsMissingAndLegacyText(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printSessionMetrics(store, []string{"missing", "--json"}, &out); err != nil || out.String() != "[]\n" {
		t.Fatalf("missing metrics = %q, %v", out.String(), err)
	}
	// Legacy rows did not record startup detail. Their unknown values must not
	// be presented as a measured zero-millisecond wait.
	started := time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC)
	if err := store.AppendMessageMetric("legacy", session.MessageMetric{Turn: 1, StartedAt: started, CompletedAt: started.Add(time.Second), DurationMS: 1000}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := printSessionMetrics(store, []string{"legacy"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "duration=1000ms") || !strings.Contains(out.String(), "first-token=unknown") {
		t.Fatalf("legacy metrics = %q", out.String())
	}
	if !strings.Contains(out.String(), "startup source=unknown") || !strings.Contains(out.String(), "firstThinking=unknown") {
		t.Fatalf("legacy startup metrics = %q", out.String())
	}
}

func TestPrintSessionMetricsStartupMilestonesAndDurations(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC)
	ms := func(n int64) *int64 { return &n }
	metric := session.MessageMetric{Turn: 1, StartedAt: started, CompletedAt: started.Add(time.Second), DurationMS: 1000,
		StartupSummary: &session.StartupSummary{RequestID: "request", Source: "server_observed", ReceivedMS: ms(0), QueueMS: ms(0),
			WorkerMS: ms(10), RuntimeMS: ms(20), RequestWrittenMS: ms(30), FirstByteMS: ms(40), FirstThinkingMS: ms(50), FirstTextMS: ms(60), TotalMS: ms(1000)}}
	if err := store.AppendMessageMetric("startup", metric); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printSessionMetrics(store, []string{"startup"}, &out); err != nil {
		t.Fatal(err)
	}
	want := "startup source=server_observed requestId=request milestones-from-api-receipt: received=0ms worker=10ms runtime=20ms mcp=unknown requestWritten=30ms firstByte=40ms firstThinking=50ms firstText=60ms total=1000ms; durations: queue=0ms executionQueue=unknown"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("startup metrics = %q, want line %q", out.String(), want)
	}
}
