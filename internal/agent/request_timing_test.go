package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/gemini"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func TestRequestTimingChatRetryAndDeltaOrder(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private thinking\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"private answer\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, collector := transport.WithTimingCollector(context.Background())
	ctx = transport.BeginTimingRequest(ctx)
	p := openai.New("private-key", server.URL, "test", 100, time.Second*5, 0)
	s, err := timedProviderStream(ctx, p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = s.Recv()
		if err != nil {
			break
		}
	}
	if err != io.EOF {
		t.Fatal(err)
	}
	_ = s.Close()
	records := collector.Records()
	phases := map[string][]transport.TimingRecord{}
	for _, r := range records {
		phases[r.Phase] = append(phases[r.Phase], r)
		if strings.Contains(fmt.Sprint(r), "private") {
			t.Fatal("timing leaked content")
		}
	}
	if len(phases["http_do_start"]) != 2 || phases["http_do_start"][0].Attempt != 1 || phases["http_do_start"][1].Attempt != 2 {
		t.Fatalf("attempts: %+v", records)
	}
	for _, phase := range []string{"http_wrote_request", "http_first_response_byte", "http_response_headers_observed", "retry_backoff_start", "retry_backoff_end", "request_slot_wait_start", "rate_cooldown_wait_end"} {
		if len(phases[phase]) == 0 {
			t.Errorf("missing %s", phase)
		}
	}
	a, b := phases["first_reasoning_delta"], phases["first_nonempty_text_delta"]
	if len(a) != 1 || len(b) != 1 || a[0].Attempt != 2 || b[0].Attempt != 2 || a[0].At.After(b[0].At) {
		t.Fatalf("first delta attribution: %+v", records)
	}
	if len(phases["stream_end"]) != 1 {
		t.Fatalf("stream end duplicated: %+v", records)
	}
}

func TestRequestTimingFakeNoBodyCancellationAndCallIsolation(t *testing.T) {
	ctx, collector := transport.WithTimingCollector(context.Background())
	ctx = transport.BeginTimingRequest(ctx)
	p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
		return &recoveryTestStream{events: []llm.StreamEvent{{Type: "thinking_delta", TextDelta: "reason"}, {Type: "text_delta", TextDelta: " \n"}}, err: context.Canceled}, nil
	}}
	s1, err := timedProviderStream(ctx, p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := timedProviderStream(ctx, p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []llm.StreamReader{s2, s1} {
		for {
			_, err = s.Recv()
			if err != nil {
				break
			}
		}
		_ = s.Close()
	}
	calls := map[int]bool{}
	for _, r := range collector.Records() {
		if r.Phase == "first_nonempty_text_delta" {
			t.Fatal("whitespace became first body")
		}
		if r.Phase == "first_reasoning_delta" {
			calls[r.ProviderCall] = true
		}
		if r.Phase == "stream_end" && r.Outcome != "canceled" {
			t.Fatalf("cancel outcome: %+v", r)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("call identities: %v", calls)
	}
}

// Close interrupts a genuinely blocked Recv, as a transport body would.
type timingBlockingStream struct {
	entered, closed chan struct{}
	once            sync.Once
}

func (s *timingBlockingStream) Recv() (llm.StreamEvent, error) {
	close(s.entered)
	<-s.closed
	return llm.StreamEvent{}, context.Canceled
}
func (s *timingBlockingStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }
func TestRequestTimingConcurrentCloseCancellation(t *testing.T) {
	ctx, c := transport.WithTimingCollector(context.Background())
	ctx = transport.BeginTimingRequest(ctx)
	ctx, cancel := context.WithCancel(ctx)
	raw := &timingBlockingStream{entered: make(chan struct{}), closed: make(chan struct{})}
	s := &timingStream{ctx: ctx, StreamReader: raw}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.Recv() }()
	<-raw.entered
	cancel()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	var n int
	for _, r := range c.Records() {
		if r.Phase == "stream_end" {
			n++
			if r.Outcome != "canceled" {
				t.Fatal(r)
			}
		}
	}
	if n != 1 {
		t.Fatalf("end count=%d", n)
	}
}

func TestRequestTimingBufferedAndHookFailure(t *testing.T) {
	traceHookMu.RLock()
	previous := traceHook
	traceHookMu.RUnlock()
	defer SetTraceHook(previous)
	var rows int
	SetTraceHook(func(ev Event) {
		if ev.Kind == EventRequestTiming {
			rows++
		}
	})
	ctx, flush := beginRunTiming(context.Background())
	ctx = transport.BeginTimingRequest(ctx)
	p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
		return &recoveryTestStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "answer"}}}, nil
	}}
	s, err := timedProviderStream(ctx, p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Recv()
	if rows != 0 {
		t.Fatal("timing hook ran on first delta")
	}
	_ = s.Close()
	flush()
	if rows == 0 {
		t.Fatal("timing not flushed")
	}
	SetTraceHook(func(Event) { panic("broken telemetry") })
	_, flush = beginRunTiming(context.Background())
	flush()
	if transport.TimingOutcome(fmt.Errorf("wrapped: %w", context.Canceled)) != "canceled" {
		t.Fatal("wrapped cancellation")
	}
	ctx, c := transport.WithTimingCollector(context.Background())
	ctx = transport.BeginTimingRequest(ctx)
	s = &timingStream{ctx: ctx, StreamReader: &recoveryTestStream{events: []llm.StreamEvent{{Type: "error", Err: errors.New("failed")}}}}
	_, _ = s.Recv()
	_ = s.Close()
	for _, r := range c.Records() {
		if r.Phase == "stream_end" && r.Outcome != "error" {
			t.Fatal(r)
		}
	}
}

func assertTimingStreamEnd(t *testing.T, c *transport.TimingCollector, outcome string) {
	t.Helper()
	var ends []transport.TimingRecord
	for _, r := range c.Records() {
		if r.Phase == "stream_end" {
			ends = append(ends, r)
		}
	}
	if len(ends) != 1 || ends[0].Outcome != outcome {
		t.Fatalf("stream_end: got %+v, want exactly one %q", ends, outcome)
	}
}

// Return a terminal event and error together, without requiring another Recv.
// The embedded recovery harness tracks closure and supplies preceding deltas.
type timingTerminalStream struct {
	recoveryTestStream
	terminal llm.StreamEvent
	err      error
	closeErr error
	calls    int
}

func (s *timingTerminalStream) Recv() (llm.StreamEvent, error) {
	s.calls++
	if s.idx < len(s.events) {
		return s.recoveryTestStream.Recv()
	}
	return s.terminal, s.err
}

func (s *timingTerminalStream) Close() error {
	_ = s.recoveryTestStream.Close()
	return s.closeErr
}

func TestRequestTimingMessageStopConsumeThenClose(t *testing.T) {
	for _, terminalErr := range []error{nil, io.EOF, fmt.Errorf("wrapped: %w", io.EOF)} {
		t.Run(fmt.Sprint(terminalErr), func(t *testing.T) {
			ctx, c := transport.WithTimingCollector(context.Background())
			ctx = transport.BeginTimingRequest(ctx)
			raw := &timingTerminalStream{
				recoveryTestStream: recoveryTestStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "answer"}}},
				terminal:           llm.StreamEvent{Type: "message_stop", StopReason: "end_turn", InputTokens: 7, OutputTokens: 2},
				err:                terminalErr,
			}
			p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) { return raw, nil }}
			s, err := timedProviderStream(ctx, p, llm.Request{})
			if err != nil {
				t.Fatal(err)
			}
			blocks, stop, usage, err := (&Loop{}).consumeStream(ctx, s, make(chan Event, 16))
			if err != nil || stop != "end_turn" || len(blocks) != 1 || blocks[0].Text != "answer" || usage.in != 7 || usage.out != 2 {
				t.Fatalf("consume: blocks=%+v stop=%q usage=%+v err=%v", blocks, stop, usage, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			if raw.calls != 2 || !raw.closed {
				t.Fatalf("calls=%d closed=%v; must close without reading EOF", raw.calls, raw.closed)
			}
			assertTimingStreamEnd(t, c, "ok")
		})
	}
}

// Exercise Gemini's actual SSE parser, not a reconstructed message_stop event.
func TestRequestTimingGeminiConsumeThenClose(t *testing.T) {
	for _, finish := range []bool{true, false} {
		name, suffix, wantStop := "cleanEOF", "", ""
		if finish {
			name, suffix, wantStop = "finishSTOP", `,"finishReason":"STOP"`, "end_turn"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"answer\"}]}%s}]}\n\n", suffix)
			}))
			defer server.Close()
			ctx, c := transport.WithTimingCollector(context.Background())
			ctx = transport.BeginTimingRequest(ctx)
			p := gemini.New("test-key", server.URL, "gemini-test", 100, 5*time.Second, 0)
			s, err := timedProviderStream(ctx, p, llm.Request{})
			if err != nil {
				t.Fatal(err)
			}
			observed := &timingObservedStream{StreamReader: s.(*timingStream).StreamReader}
			s.(*timingStream).StreamReader = observed
			blocks, stop, _, err := (&Loop{}).consumeStream(ctx, s, make(chan Event, 16))
			if err != nil || stop != wantStop || len(blocks) != 1 || blocks[0].Text != "answer" {
				t.Fatalf("consume: blocks=%+v stop=%q err=%v", blocks, stop, err)
			}
			if observed.last.Type != "message_stop" || observed.err != nil {
				t.Fatalf("terminal Recv: event=%+v err=%v", observed.last, observed.err)
			}
			assertTimingStreamEnd(t, c, "ok")
			calls := observed.calls
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if observed.calls != calls {
				t.Fatal("Close read beyond message_stop")
			}
			assertTimingStreamEnd(t, c, "ok")
		})
	}
}

type timingObservedStream struct {
	llm.StreamReader
	last  llm.StreamEvent
	err   error
	calls int
}

func (s *timingObservedStream) Recv() (llm.StreamEvent, error) {
	s.calls++
	s.last, s.err = s.StreamReader.Recv()
	return s.last, s.err
}

func TestRequestTimingTerminalPriorityAndEarlyClose(t *testing.T) {
	failure := errors.New("stream failed")
	for _, tc := range []struct {
		name, kind, outcome string
		err, eventErr       error
		closeErr            error
		cancel, earlyClose  bool
	}{
		{name: "stopWithError", kind: "message_stop", err: failure, outcome: "error"},
		{name: "stopWithCancel", kind: "message_stop", err: context.Canceled, outcome: "canceled"},
		{name: "errorEvent", kind: "error", eventErr: failure, outcome: "error"},
		{name: "errorEventWithoutCause", kind: "error", outcome: "error"},
		{name: "recvCancelBeforeEventError", kind: "error", err: context.Canceled, eventErr: failure, outcome: "canceled"},
		{name: "recvEOFBeforeEventError", kind: "error", err: io.EOF, eventErr: failure, outcome: "ok"},
		{name: "plainEOF", err: io.EOF, outcome: "ok"},
		{name: "earlyClose", earlyClose: true, outcome: "closed"},
		{name: "earlyCloseError", earlyClose: true, closeErr: failure, outcome: "error"},
		{name: "canceledClose", earlyClose: true, cancel: true, closeErr: failure, outcome: "canceled"},
		{name: "stopBeforeCloseError", kind: "message_stop", closeErr: failure, outcome: "ok"},
		{name: "stopBeforeCanceledClose", kind: "message_stop", cancel: true, outcome: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, c := transport.WithTimingCollector(context.Background())
			ctx, cancel := context.WithCancel(transport.BeginTimingRequest(ctx))
			defer cancel()
			raw := &timingTerminalStream{terminal: llm.StreamEvent{Type: tc.kind, Err: tc.eventErr}, err: tc.err, closeErr: tc.closeErr}
			s := &timingStream{ctx: ctx, StreamReader: raw}
			if !tc.earlyClose {
				ev, err := s.Recv()
				if ev.Type != tc.kind || ev.Err != tc.eventErr || err != tc.err {
					t.Fatalf("Recv changed: event=%+v err=%v", ev, err)
				}
				assertTimingStreamEnd(t, c, tc.outcome)
			}
			if tc.cancel {
				cancel()
			}
			for range 2 {
				if err := s.Close(); err != tc.closeErr {
					t.Fatalf("Close changed: got %v want %v", err, tc.closeErr)
				}
			}
			assertTimingStreamEnd(t, c, tc.outcome)
		})
	}
}
