package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TimingRecord contains only locally generated identities, fixed phase/outcome
// labels and numeric metadata. At is the occurrence time, not the flush time.
// ElapsedMS is measured with Go's monotonic clock and deliberately keeps zero.
// Records are buffered until the owning agent Run exits; they are not UI TTFT.
type TimingRecord struct {
	Phase        string    `json:"phase"`
	RequestID    string    `json:"request_id"`
	ProviderCall int       `json:"provider_call"`
	Attempt      int       `json:"attempt"`
	ElapsedMS    int64     `json:"elapsed_ms"`
	At           time.Time `json:"at"`
	StatusCode   int       `json:"status_code,omitempty"`
	Outcome      string    `json:"outcome,omitempty"`
}

type TimingCollector struct {
	mu      sync.Mutex
	records []TimingRecord
}
type timingKey struct{}
type timingScope struct {
	collector *TimingCollector
	requestID string
	started   time.Time
	mu        sync.Mutex
	calls     int
	call      int
	attempt   int
}

// WithTimingCollector shadows any parent agent's collector.
func WithTimingCollector(ctx context.Context) (context.Context, *TimingCollector) {
	c := &TimingCollector{}
	return context.WithValue(ctx, timingKey{}, &timingScope{collector: c}), c
}

func timingFrom(ctx context.Context) *timingScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(timingKey{}).(*timingScope)
	return s
}

func TimingEnabled(ctx context.Context) bool { return timingFrom(ctx) != nil }

func BeginTimingRequest(ctx context.Context) context.Context {
	s := timingFrom(ctx)
	if s == nil {
		return ctx
	}
	s = &timingScope{collector: s.collector, requestID: uuid.NewString(), started: time.Now()}
	ctx = context.WithValue(ctx, timingKey{}, s)
	RecordTiming(ctx, "pre_request_start")
	return ctx
}

func BeginTimingCall(ctx context.Context) context.Context {
	s := timingFrom(ctx)
	if s == nil {
		return ctx
	}
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	return context.WithValue(ctx, timingKey{}, &timingScope{collector: s.collector, requestID: s.requestID, started: s.started, call: call})
}

func (c *TimingCollector) Records() []TimingRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]TimingRecord(nil), c.records...)
}

func RecordTiming(ctx context.Context, phase string) { RecordTimingResult(ctx, phase, "", 0) }
func RecordTimingResult(ctx context.Context, phase, outcome string, status int) {
	s := timingFrom(ctx)
	if s == nil || s.started.IsZero() {
		return
	}
	now := time.Now()
	s.mu.Lock()
	r := TimingRecord{Phase: phase, RequestID: s.requestID, ProviderCall: s.call, Attempt: s.attempt, ElapsedMS: now.Sub(s.started).Milliseconds(), At: now, StatusCode: status, Outcome: outcome}
	s.mu.Unlock()
	s.collector.mu.Lock()
	s.collector.records = append(s.collector.records, r)
	s.collector.mu.Unlock()
}

// SnapshotTiming binds a returned stream to the HTTP attempt that opened it,
// even if a later recovery call opens a replacement stream.
func SnapshotTiming(ctx context.Context) context.Context {
	s := timingFrom(ctx)
	if s == nil {
		return ctx
	}
	s.mu.Lock()
	copy := &timingScope{collector: s.collector, requestID: s.requestID, started: s.started, call: s.call, attempt: s.attempt}
	s.mu.Unlock()
	return context.WithValue(ctx, timingKey{}, copy)
}

func TimingOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "error"
}

// Do observes the client.Do boundary, not socket write or first response byte.
// http_response_headers_observed means Do returned headers, including any
// outer wrapper overhead; http_first_response_byte is the httptrace boundary.
// Redirects and net/http internal connection replays remain client-owned.
// Neither request URLs, headers, bodies nor error strings are retained.
func Do(client *http.Client, req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if s := timingFrom(ctx); s != nil {
		s.mu.Lock()
		s.attempt++
		s.mu.Unlock()
	}
	// Compose with existing hooks. These observe the actual transport boundary
	// even when outer usage/debug wrappers do work before Do returns.
	if timingFrom(ctx) != nil {
		traceCtx := SnapshotTiming(ctx)
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				RecordTimingResult(traceCtx, "http_wrote_request", TimingOutcome(info.Err), 0)
			},
			GotFirstResponseByte: func() { RecordTiming(traceCtx, "http_first_response_byte") },
		}))
	}
	RecordTiming(ctx, "http_do_start")
	resp, err := client.Do(req)
	if resp != nil {
		RecordTimingResult(ctx, "http_response_headers_observed", TimingOutcome(err), resp.StatusCode)
	} else if err != nil {
		RecordTimingResult(ctx, "http_error", TimingOutcome(err), 0)
	}
	return resp, err
}
