// Package telemetry exports metis usage metrics over OTLP/HTTP (JSON
// encoding) to an OpenTelemetry collector. Dependency-free: rather than
// pull in the full OTEL SDK (several modules), it hand-rolls the small
// subset of the OTLP metrics JSON schema metis needs — a set of monotonic
// sum counters plus a last-value gauge — and POSTs them to
// <endpoint>/v1/metrics.
//
// Activation is the standard OTEL env var: set OTEL_EXPORTER_OTLP_ENDPOINT
// (e.g. http://localhost:4318) and an Exporter is created; unset → New
// returns nil and every method is a no-op, so callers need no guards.
//
// Metrics emitted (all attributed with model + session.id):
//
//	metis.tokens.input / output / cache_read / cache_create  (sum, monotonic)
//	metis.tool.calls / tool.errors                            (sum, monotonic)
//	metis.turns                                               (sum, monotonic)
//	metis.turn.duration_ms                                    (gauge, last)
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TurnMetrics is one round-trip's measurements, handed to RecordTurn.
type TurnMetrics struct {
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheCreate  int
	ToolCalls    int
	ToolErrors   int
	DurationMS   int64
}

// Exporter accumulates counters across turns and flushes them as OTLP
// metrics. Safe for concurrent use. A nil *Exporter is a valid no-op.
type Exporter struct {
	endpoint string // full URL, e.g. http://host:4318/v1/metrics
	model    string
	session  string
	client   *http.Client

	mu            sync.Mutex
	tokIn         int64
	tokOut        int64
	cacheRead     int64
	cacheCreate   int64
	toolCalls     int64
	toolErrors    int64
	turns         int64
	lastDurMS     int64
	hasObservatns bool
}

// New returns an Exporter when OTEL_EXPORTER_OTLP_ENDPOINT is set, else
// nil (a no-op). model/session tag every data point. The endpoint may be
// the base (http://host:4318) — /v1/metrics is appended — or already
// include the path.
func New(model, session string) *Exporter {
	ep := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if ep == "" {
		return nil
	}
	ep = strings.TrimRight(ep, "/")
	if !strings.HasSuffix(ep, "/v1/metrics") {
		ep += "/v1/metrics"
	}
	return &Exporter{
		endpoint: ep,
		model:    model,
		session:  session,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// RecordTurn folds one turn's metrics into the running counters.
func (e *Exporter) RecordTurn(m TurnMetrics) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.tokIn += int64(m.InputTokens)
	e.tokOut += int64(m.OutputTokens)
	e.cacheRead += int64(m.CacheRead)
	e.cacheCreate += int64(m.CacheCreate)
	e.toolCalls += int64(m.ToolCalls)
	e.toolErrors += int64(m.ToolErrors)
	e.turns++
	e.lastDurMS = m.DurationMS
	e.hasObservatns = true
	e.mu.Unlock()
}

// Export POSTs the current cumulative metrics as OTLP/HTTP JSON. Safe to
// call repeatedly (counters are monotonic cumulative). No-op when nil or
// nothing has been recorded.
func (e *Exporter) Export(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if !e.hasObservatns {
		e.mu.Unlock()
		return nil
	}
	payload := e.buildPayload()
	e.mu.Unlock()

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// buildPayload constructs the OTLP ResourceMetrics JSON. Caller holds mu.
func (e *Exporter) buildPayload() map[string]any {
	nowNano := strconv.FormatInt(time.Now().UnixNano(), 10)
	attrs := []map[string]any{
		strAttr("model", e.model),
		strAttr("session.id", e.session),
	}
	sum := func(name string, v int64) map[string]any {
		return map[string]any{
			"name": name,
			"sum": map[string]any{
				"aggregationTemporality": 2, // CUMULATIVE
				"isMonotonic":            true,
				"dataPoints": []map[string]any{{
					"asInt":        strconv.FormatInt(v, 10),
					"timeUnixNano": nowNano,
					"attributes":   attrs,
				}},
			},
		}
	}
	gauge := func(name string, v int64) map[string]any {
		return map[string]any{
			"name": name,
			"gauge": map[string]any{
				"dataPoints": []map[string]any{{
					"asInt":        strconv.FormatInt(v, 10),
					"timeUnixNano": nowNano,
					"attributes":   attrs,
				}},
			},
		}
	}
	metrics := []map[string]any{
		sum("metis.tokens.input", e.tokIn),
		sum("metis.tokens.output", e.tokOut),
		sum("metis.tokens.cache_read", e.cacheRead),
		sum("metis.tokens.cache_create", e.cacheCreate),
		sum("metis.tool.calls", e.toolCalls),
		sum("metis.tool.errors", e.toolErrors),
		sum("metis.turns", e.turns),
		gauge("metis.turn.duration_ms", e.lastDurMS),
	}
	return map[string]any{
		"resourceMetrics": []map[string]any{{
			"resource": map[string]any{
				"attributes": []map[string]any{strAttr("service.name", "metis")},
			},
			"scopeMetrics": []map[string]any{{
				"scope":   map[string]any{"name": "metis"},
				"metrics": metrics,
			}},
		}},
	}
}

func strAttr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}
