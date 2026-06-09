package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew_NilWhenUnset(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if e := New("m", "s"); e != nil {
		t.Error("expected nil exporter when endpoint unset")
	}
	// nil exporter methods must be safe no-ops.
	var e *Exporter
	e.RecordTurn(TurnMetrics{InputTokens: 1})
	if err := e.Export(context.Background()); err != nil {
		t.Errorf("nil Export should be a no-op, got %v", err)
	}
}

func TestExporter_EndpointPathAppended(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://host:4318")
	e := New("m", "s")
	if e == nil || !strings.HasSuffix(e.endpoint, "/v1/metrics") {
		t.Fatalf("endpoint = %q, want .../v1/metrics", e.endpoint)
	}
	// Already-pathed endpoint isn't double-suffixed.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://host:4318/v1/metrics")
	e2 := New("m", "s")
	if strings.Count(e2.endpoint, "/v1/metrics") != 1 {
		t.Errorf("double-suffixed endpoint: %q", e2.endpoint)
	}
}

func TestExporter_RecordAndExport(t *testing.T) {
	var gotBody []byte
	var gotCT, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	e := New("deepseek-v4", "sess-1")
	if e == nil {
		t.Fatal("expected an exporter")
	}
	e.RecordTurn(TurnMetrics{InputTokens: 100, OutputTokens: 20, ToolCalls: 3, ToolErrors: 1, DurationMS: 1500})
	e.RecordTurn(TurnMetrics{InputTokens: 50, OutputTokens: 10, ToolCalls: 2, DurationMS: 800})
	if err := e.Export(context.Background()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if gotPath != "/v1/metrics" {
		t.Errorf("path = %q, want /v1/metrics", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}

	// Parse the OTLP payload and check the summed counters + attributes.
	var p map[string]any
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload not JSON: %v\n%s", err, gotBody)
	}
	body := string(gotBody)
	for _, want := range []string{
		"metis.tokens.input", "metis.tokens.output", "metis.tool.calls",
		"metis.tool.errors", "metis.rounds", "metis.round.duration_ms",
		"deepseek-v4", "sess-1", "service.name", "metis",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("payload missing %q", want)
		}
	}
	// Summed: input 100+50=150, turns=2, last duration=800.
	if !strings.Contains(body, `"150"`) {
		t.Errorf("expected summed input tokens 150 in payload")
	}
	if !strings.Contains(body, `"800"`) {
		t.Errorf("expected last duration 800 in payload")
	}
}

func TestExporter_NoExportWhenEmpty(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	e := New("m", "s")
	if err := e.Export(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Error("Export with no recorded turns should not POST")
	}
}
