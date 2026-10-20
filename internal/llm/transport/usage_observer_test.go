package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type usageTestObserver struct {
	mu          sync.Mutex
	metadata    []RequestMetadata
	requests    []*usageTestRequest
	beginErr    error
	headerErr   error
	snapshotErr error
	endErr      error
}
type usageTestRequest struct {
	owner     *usageTestObserver
	headers   []string
	snapshots []UsageSnapshot
	statuses  []UsageStatus
}

func (o *usageTestObserver) Begin(_ context.Context, meta RequestMetadata) (RequestUsageObserver, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.beginErr != nil {
		return nil, o.beginErr
	}
	r := &usageTestRequest{owner: o}
	o.metadata = append(o.metadata, meta)
	o.requests = append(o.requests, r)
	return r, nil
}
func (r *usageTestRequest) Header(id string) error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.headerErr != nil {
		return r.owner.headerErr
	}
	r.headers = append(r.headers, id)
	return nil
}
func (r *usageTestRequest) Snapshot(snapshot UsageSnapshot) error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.snapshotErr != nil {
		return r.owner.snapshotErr
	}
	r.snapshots = append(r.snapshots, snapshot)
	return nil
}
func (r *usageTestRequest) End(status UsageStatus) error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.endErr != nil {
		return r.owner.endErr
	}
	r.statuses = append(r.statuses, status)
	return nil
}

type usageTestRT func(*http.Request) (*http.Response, error)

func (f usageTestRT) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func usageTestHTTP(t *testing.T, observer *usageTestObserver, stream bool) *http.Request {
	t.Helper()
	body := `{"model":"test-model","messages":[{"content":"private prompt"}],"stream":false}`
	if stream {
		body = strings.Replace(body, `false`, `true`, 1)
	}
	req, err := http.NewRequestWithContext(WithUsageObserver(context.Background(), observer), "POST", "https://user:password@api.example.com/v1/messages?key=secret#private", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-auth")
	req.Header.Set(HeaderRequestID, "caller-id")
	return req
}
func usageTestResponse(body io.ReadCloser, stream bool) *http.Response {
	header := make(http.Header)
	header.Set("request-id", "upstream-request")
	if stream {
		header.Set("Content-Type", "text/event-stream")
	} else {
		header.Set("Content-Type", "application/json")
	}
	return &http.Response{StatusCode: 200, Header: header, Body: body}
}
func usageFrame(payload string) string { return "data: " + payload + "\n\n" }

func TestHasUsageObserverIsOptionalAndPropagatesThroughDerivedContext(t *testing.T) {
	if HasUsageObserver(nil) || HasUsageObserver(context.Background()) || HasUsageObserver(WithUsageObserver(context.Background(), nil)) {
		t.Fatal("missing observer reported as present")
	}
	ctx := WithUsageObserver(context.Background(), &usageTestObserver{})
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if !HasUsageObserver(ctx) || !HasUsageObserver(child) {
		t.Fatal("observer lost across derived request context")
	}
}

func TestUsageObserverUnsupportedProtocolFailsBeforeDispatch(t *testing.T) {
	for _, protocol := range []string{"gemini", "bedrock", "vertex", "unknown"} {
		o := &usageTestObserver{}
		called := false
		rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), protocol, nil)
		_, err := rt.RoundTrip(usageTestHTTP(t, o, true))
		var failure *UsageObserverError
		if called || len(o.requests) != 0 || !errors.As(err, &failure) || failure.Stage != "unsupported_protocol" || shouldRetry(err) {
			t.Fatalf("unsupported provider emitted an unaccounted RPC: protocol=%s called=%v err=%v", protocol, called, err)
		}
	}
}

func TestUsageObserverBeginBeforeDispatchAndPhysicalRetryIdentity(t *testing.T) {
	o := &usageTestObserver{}
	req := usageTestHTTP(t, o, true)
	dispatches := 0
	ids := make(map[string]bool)
	rt := WrapRoundTripper(usageTestRT(func(actual *http.Request) (*http.Response, error) {
		dispatches++
		if len(o.metadata) != dispatches {
			t.Fatal("network dispatched before durable begin")
		}
		meta := o.metadata[dispatches-1]
		id := actual.Header.Get(HeaderRequestID)
		if len(id) != 32 || id != meta.ClientRequestID || ids[id] {
			t.Fatalf("physical request identity reused: %q", id)
		}
		ids[id] = true
		if meta.Model != "test-model" || meta.Provider != "anthropic" {
			t.Fatalf("bad metadata: %+v", meta)
		}
		canonical := sha256.Sum256([]byte("https://api.example.com/v1/messages"))
		if meta.EndpointSHA256 != hex.EncodeToString(canonical[:]) {
			t.Fatal("endpoint fingerprint includes credentials/query")
		}
		raw, err := io.ReadAll(actual.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantHash := sha256.Sum256(raw)
		if meta.RequestSHA256 != hex.EncodeToString(wantHash[:]) || !bytes.Contains(raw, []byte("private prompt")) {
			t.Fatal("request body changed during metadata capture")
		}
		return usageTestResponse(io.NopCloser(strings.NewReader(usageFrame(`{"type":"message_stop"}`))), true), nil
	}), "anthropic", nil)
	for i := 0; i < 2; i++ {
		if i > 0 {
			req.Body, _ = req.GetBody()
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if req.Header.Get(HeaderRequestID) != "caller-id" {
		t.Fatal("observer mutated caller header")
	}
	if len(o.requests) != 2 || o.requests[0].statuses[0] != UsageCompleted {
		t.Fatal("physical attempts not completed separately")
	}
}

func TestUsageObserverBeginFailureCannotSendOrBeRetriedAsNetwork(t *testing.T) {
	o := &usageTestObserver{beginErr: io.EOF}
	called := false
	rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), "anthropic", nil)
	_, err := rt.RoundTrip(usageTestHTTP(t, o, true))
	var observerErr *UsageObserverError
	if called || !errors.As(err, &observerErr) || observerErr.Stage != "begin" || shouldRetry(err) || strings.Contains(err.Error(), "EOF") {
		t.Fatalf("begin failure crossed network or leaked cause: called=%v err=%v", called, err)
	}
}

func TestUsageObserverThreeStreamingProtocolsAbsoluteSnapshots(t *testing.T) {
	cases := []struct {
		name, protocol, raw string
		want                UsageSnapshot
		count               int
	}{
		{"anthropic", "anthropic", usageFrame(`{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":100,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}}`) + usageFrame(`{"type":"message_delta","usage":{"output_tokens":7}}`) + usageFrame(`{"type":"message_delta","usage":{"output_tokens":7}}`) + usageFrame(`{"type":"message_stop"}`), UsageSnapshot{ResponseID: "msg-1", UsageReported: true, InputTokens: 100, OutputTokens: 7, CacheReadInputTokens: 20, CacheCreationInputTokens: 5}, 2},
		{"chat", "openai", usageFrame(`{"id":"chat-1","choices":[{"delta":{"content":"hello"}}]}`) + usageFrame(`{"id":"chat-1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":20}}}`) + usageFrame(`{"id":"chat-1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":20}}}`) + usageFrame(`[DONE]`), UsageSnapshot{ResponseID: "chat-1", UsageReported: true, InputTokens: 80, OutputTokens: 7, CacheReadInputTokens: 20}, 2},
		{"responses", "openai-responses", usageFrame(`{"type":"response.created","response":{"id":"resp-1"}}`) + usageFrame(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":20}}}}`), UsageSnapshot{ResponseID: "resp-1", UsageReported: true, InputTokens: 80, OutputTokens: 7, CacheReadInputTokens: 20}, 2},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			o := &usageTestObserver{}
			// Tiny source reads split both JSON metadata and frame delimiters.
			rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) {
				return usageTestResponse(&usageChunkReader{data: []byte(test.raw), chunk: 3}, true), nil
			}), test.protocol, nil)
			resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != test.raw {
				t.Fatal("observer changed provider wire bytes")
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			r := o.requests[0]
			if len(r.snapshots) != test.count || r.snapshots[len(r.snapshots)-1] != test.want {
				t.Fatalf("absolute snapshot was lost/doubled: %+v want=%+v", r.snapshots, test.want)
			}
			if len(r.statuses) != 1 || r.statuses[0] != UsageCompleted || len(r.headers) != 1 || r.headers[0] != "upstream-request" {
				t.Fatalf("wrong request lifecycle: %+v", r)
			}
		})
	}
}

type usageChunkReader struct {
	data    []byte
	chunk   int
	tailErr error
	closed  bool
}

func (r *usageChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if r.tailErr != nil {
			return 0, r.tailErr
		}
		return 0, io.EOF
	}
	n := len(p)
	if n > r.chunk {
		n = r.chunk
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}
func (r *usageChunkReader) Close() error { r.closed = true; return nil }

func TestUsageObserverCheckpointBeforeReleasingFrame(t *testing.T) {
	o := &usageTestObserver{}
	first := usageFrame(`{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":19}}}`)
	second := usageFrame(`{"type":"message_delta","usage":{"output_tokens":3}}`)
	source := &usageChunkReader{data: []byte(first + second), chunk: 4}
	rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) { return usageTestResponse(source, true), nil }), "anthropic", nil)
	resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 1)
	if n, err := resp.Body.Read(p); n != 1 || err != nil {
		t.Fatalf("read: %d %v", n, err)
	}
	if len(o.requests[0].snapshots) != 1 || o.requests[0].snapshots[0].InputTokens != 19 {
		t.Fatal("frame bytes released before input usage durability")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !source.closed || len(o.requests[0].statuses) != 1 || o.requests[0].statuses[0] != UsageInterrupted {
		t.Fatal("partial Close fabricated completed response")
	}
}

func TestUsageObserverSnapshotFailureDiscardsFrameAndClosesSource(t *testing.T) {
	o := &usageTestObserver{snapshotErr: errors.New("disk contains secret-auth")}
	source := &usageChunkReader{data: []byte(usageFrame(`{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":19}}}`)), chunk: 4}
	rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) { return usageTestResponse(source, true), nil }), "anthropic", nil)
	resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 1024)
	n, err := resp.Body.Read(p)
	var failure *UsageObserverError
	if n != 0 || !errors.As(err, &failure) || failure.Stage != "snapshot" || !source.closed || shouldRetry(err) || strings.Contains(err.Error(), "secret-auth") {
		t.Fatalf("failed persistence released bytes/retried/leaked: n=%d err=%v closed=%v", n, err, source.closed)
	}
	if len(o.requests[0].statuses) != 1 || o.requests[0].statuses[0] != UsageInterrupted {
		t.Fatal("snapshot failure was hidden")
	}
}

func TestUsageObserverHeaderFailureStopsBeforeReading(t *testing.T) {
	o := &usageTestObserver{headerErr: errors.New("ledger unavailable")}
	source := &usageChunkReader{data: []byte("must not be read"), chunk: 1}
	rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) { return usageTestResponse(source, true), nil }), "anthropic", nil)
	resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
	var failure *UsageObserverError
	if resp != nil || !errors.As(err, &failure) || len(source.data) != len("must not be read") || !source.closed {
		t.Fatalf("header persistence failure continued: resp=%v err=%v", resp, err)
	}
}

func TestUsageObserverReadErrorRetainsReportedPartialUsage(t *testing.T) {
	o := &usageTestObserver{}
	raw := usageFrame(`{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":19}}}`)
	source := &usageChunkReader{data: []byte(raw), chunk: 5, tailErr: io.ErrUnexpectedEOF}
	rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) { return usageTestResponse(source, true), nil }), "anthropic", nil)
	resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if string(got) != raw || !errors.Is(err, io.ErrUnexpectedEOF) || len(o.requests[0].snapshots) != 1 || o.requests[0].statuses[0] != UsageInterrupted {
		t.Fatalf("partial usage lost: got=%q err=%v request=%+v", got, err, o.requests[0])
	}
	_ = resp.Body.Close()
}

func TestUsageObserverNonStreamingBeforeBytesAndExplicitZero(t *testing.T) {
	for _, protocol := range []string{"anthropic", "openai", "openai-responses"} {
		t.Run(protocol, func(t *testing.T) {
			o := &usageTestObserver{}
			raw := `{"id":"complete-1","content":[],"usage":{"input_tokens":19,"output_tokens":0}}`
			if protocol == "openai" {
				raw = `{"id":"complete-1","choices":[],"usage":{"prompt_tokens":19,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":4}}}`
			}
			if protocol == "openai-responses" {
				raw = `{"id":"complete-1","status":"completed","usage":{"input_tokens":19,"output_tokens":0,"input_tokens_details":{"cached_tokens":4}}}`
			}
			rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) {
				return usageTestResponse(io.NopCloser(strings.NewReader(raw)), false), nil
			}), protocol, nil)
			resp, err := rt.RoundTrip(usageTestHTTP(t, o, false))
			if err != nil {
				t.Fatal(err)
			}
			p := make([]byte, 1)
			if n, err := resp.Body.Read(p); n != 1 || err != nil {
				t.Fatal(n, err)
			}
			r := o.requests[0]
			if len(r.snapshots) != 1 || r.snapshots[0].OutputTokens != 0 || len(r.statuses) != 1 || r.statuses[0] != UsageCompleted {
				t.Fatalf("nonstream bytes crossed durability first: %+v", r)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestUsageObserverRetainsAbsentFieldsAndNormalizesLateCachedInput(t *testing.T) {
	o := &usageTestObserver{}
	raw := usageFrame(`{"id":"chat-1","usage":{"prompt_tokens":100,"completion_tokens":7}}`) + usageFrame(`{"id":"chat-1","usage":{"prompt_tokens_details":{"cached_tokens":20}}}`) + usageFrame(`{"id":"chat-1","usage":{"completion_tokens":0}}`) + usageFrame(`[DONE]`)
	rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) {
		return usageTestResponse(io.NopCloser(strings.NewReader(raw)), true), nil
	}), "openai", nil)
	resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	s := o.requests[0].snapshots
	if len(s) != 3 || s[1].InputTokens != 80 || s[1].OutputTokens != 7 || s[1].CacheReadInputTokens != 20 || s[2].InputTokens != 80 || s[2].OutputTokens != 0 {
		t.Fatalf("missing versus explicit-zero usage collapsed: %+v", s)
	}
	_ = resp.Body.Close()
}

func TestUsageObserverOpenAIFlatCacheFieldsMatchProviderPriority(t *testing.T) {
	for _, test := range []struct {
		usage string
		want  int64
	}{
		{`{"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":20}`, 20},
		{`{"prompt_tokens":100,"completion_tokens":7,"cached_tokens":30}`, 30},
		{`{"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":20,"cached_tokens":30}`, 20},
		{`{"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}`, 40},
		{`{"prompt_tokens":100,"completion_tokens":7,"prompt_cache_hit_tokens":20,"prompt_tokens_details":{"cached_tokens":0}}`, 20},
	} {
		o := &usageTestObserver{}
		raw := usageFrame(`{"id":"chat-1","usage":`+test.usage+`}`) + usageFrame(`[DONE]`)
		rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) {
			return usageTestResponse(io.NopCloser(strings.NewReader(raw)), true), nil
		}), "openai", nil)
		resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		got := o.requests[0].snapshots[0]
		if got.CacheReadInputTokens != test.want || got.InputTokens != 100-test.want {
			t.Fatalf("flat cache normalization differs from provider: got=%+v wantCache=%d", got, test.want)
		}
		_ = resp.Body.Close()
	}
}

func TestUsageObserverInvalidCountersIdentityAndFrameBound(t *testing.T) {
	for _, raw := range []string{
		usageFrame(`{"id":"a","usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`),
		usageFrame(`{"id":"a","usage":{"prompt_tokens":-1}}`),
		usageFrame(`{"id":"a","usage":{"prompt_tokens":9223372036854775808}}`),
		usageFrame(`{"id":"` + strings.Repeat("x", usageIdentityLimit+1) + `"}`),
		usageFrame(`{"id":"a"}`) + usageFrame(`{"id":"b"}`),
		"data: " + strings.Repeat("x", usageFrameLimit+1) + "\n\n",
	} {
		o := &usageTestObserver{}
		rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) {
			return usageTestResponse(io.NopCloser(strings.NewReader(raw)), true), nil
		}), "openai", nil)
		resp, err := rt.RoundTrip(usageTestHTTP(t, o, true))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		var failure *UsageObserverError
		if !errors.As(err, &failure) || o.requests[0].statuses[0] != UsageInterrupted {
			t.Fatalf("invalid response passed durable observer: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestUsageObserverRejectedAndNetworkErrorEnd(t *testing.T) {
	for _, network := range []bool{false, true} {
		o := &usageTestObserver{}
		rt := WrapRoundTripper(usageTestRT(func(*http.Request) (*http.Response, error) {
			if network {
				return nil, io.ErrUnexpectedEOF
			}
			resp := usageTestResponse(io.NopCloser(strings.NewReader(`{"error":"denied"}`)), false)
			resp.StatusCode = 401
			return resp, nil
		}), "anthropic", nil)
		resp, err := rt.RoundTrip(usageTestHTTP(t, o, false))
		want := UsageRejected
		if network {
			want = UsageInterrupted
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
		if len(o.requests[0].statuses) != 1 || o.requests[0].statuses[0] != want || len(o.requests[0].snapshots) != 0 {
			t.Fatalf("request failure invented usage/completion: %+v", o.requests[0])
		}
	}
}
