package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"golang.org/x/net/http2"
)

// recoveryTestProvider deliberately does not perform provider-level retries:
// a Loop recovery session must also bound custom providers. Every Stream call
// models a successful HTTP 200 handshake followed by the supplied SSE reader.
type recoveryTestProvider struct {
	t        *testing.T
	requests []llm.Request
	stream   func(context.Context, int) (llm.StreamReader, error)
}

func (*recoveryTestProvider) Name() string          { return "stream-recovery-test" }
func (*recoveryTestProvider) ModelID() string       { return "test-model" }
func (*recoveryTestProvider) MaxContextTokens() int { return 200_000 }
func (*recoveryTestProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected non-stream request")
}
func (p *recoveryTestProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	// Request messages contain mutable maps/slices. Snapshot now so a later
	// history mutation cannot make a broken retry appear identical retrospectively.
	data, err := json.Marshal(req)
	if err != nil {
		p.t.Fatal(err)
	}
	var snapshot llm.Request
	if err := json.Unmarshal(data, &snapshot); err != nil {
		p.t.Fatal(err)
	}
	p.requests = append(p.requests, snapshot)
	return p.stream(ctx, len(p.requests))
}

type recoveryTestStream struct {
	events  []llm.StreamEvent
	err     error
	idx     int
	closed  bool
	onError func()
}

func (s *recoveryTestStream) Recv() (llm.StreamEvent, error) {
	if s.idx < len(s.events) {
		ev := s.events[s.idx]
		s.idx++
		return ev, nil
	}
	if s.onError != nil {
		s.onError()
		s.onError = nil
	}
	if s.err != nil {
		return llm.StreamEvent{}, s.err
	}
	return llm.StreamEvent{}, io.EOF
}
func (s *recoveryTestStream) Close() error { s.closed = true; return nil }

type recoveryTestTool struct {
	tools.BaseTool
	calls atomic.Int32
}

func (*recoveryTestTool) Name() string        { return "RecoveryProbe" }
func (*recoveryTestTool) Description() string { return "record one authorized execution" }
func (*recoveryTestTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object", "required": []string{"value"},
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
	}
}
func (*recoveryTestTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (*recoveryTestTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (p *recoveryTestTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	p.calls.Add(1)
	return &tools.Result{Output: "RECORDED_ONCE"}, nil
}

func configureStreamRecoveryTest(t *testing.T, enabled bool) {
	t.Helper()
	seconds := "0"
	if enabled {
		seconds = "600"
	}
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", seconds)
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "3")
	t.Setenv("METIS_RECOVERY_MAX_BACKOFF_SECONDS", "1")
	t.Setenv("METIS_TURN_MAX_SECONDS", "0")
}

func configureDefaultStreamRecoveryTest(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "")
	t.Setenv("METIS_RECOVERY_MAX_BACKOFF_SECONDS", "")
	t.Setenv("METIS_TURN_MAX_SECONDS", "0")
}

func TestLoopDefaultStreamRecoveryHTTP2KeepsToolsExactlyOnce(t *testing.T) {
	for _, closedTool := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed_tool_%t", closedTool), func(t *testing.T) {
			configureDefaultStreamRecoveryTest(t)
			synctest.Test(t, func(t *testing.T) {
				probe := &recoveryTestTool{}
				p := &recoveryTestProvider{t: t}
				p.stream = func(_ context.Context, call int) (llm.StreamReader, error) {
					switch call {
					case 1:
						return toolUseStream("completed-before-reset", "RecoveryProbe", `{"value":"once"}`), nil
					case 2:
						cut := incompleteRecoveryToolStream(closedTool)
						cut.err = http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}
						return cut, nil
					case 3:
						return textStream("completed tool result acknowledged"), nil
					default:
						return nil, errors.New("unexpected request")
					}
				}
				loop := newStreamRecoveryTestLoop(p, probe)
				if err := loop.Run(context.Background(), make(chan Event, 128)); err != nil {
					t.Fatalf("default HTTP/2 recovery failed: %v", err)
				}
				if len(p.requests) != 3 || probe.calls.Load() != 1 {
					t.Fatalf("unexpected calls: requests=%d tools=%d", len(p.requests), probe.calls.Load())
				}
				if !reflect.DeepEqual(p.requests[1], p.requests[2]) {
					t.Fatal("recovery must replay only the model request, including the completed tool result")
				}
				assertNoUncommittedRecoveryHistory(t, loop.History())
			})
		})
	}
}

func TestLoopDefaultStreamRecoveryHTTP2HasOneAttemptBudget(t *testing.T) {
	configureDefaultStreamRecoveryTest(t)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			cut := incompleteRecoveryToolStream(true)
			cut.err = http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}
			return cut, nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		err := loop.Run(context.Background(), make(chan Event, 256))
		var exhausted *transport.RetryExhaustedError
		if !errors.As(err, &exhausted) || exhausted.Attempts != 3 || len(p.requests) != 3 || probe.calls.Load() != 0 {
			t.Fatalf("default recovery must stop after three total attempts: requests=%d tools=%d error=%v", len(p.requests), probe.calls.Load(), err)
		}
	})
}

func TestLoopDefaultStreamRecoveryHTTP2CancellationInterruptsBackoff(t *testing.T) {
	configureDefaultStreamRecoveryTest(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			cut := incompleteRecoveryToolStream(true)
			cut.err = http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}
			return cut, nil
		}}
		cancelDone := make(chan struct{})
		go func() { defer close(cancelDone); time.Sleep(100 * time.Millisecond); cancel() }()
		start := time.Now()
		err := newStreamRecoveryTestLoop(p, probe).Run(ctx, make(chan Event, 128))
		elapsed := time.Since(start)
		<-cancelDone
		if !errors.Is(err, context.Canceled) || elapsed != 100*time.Millisecond || len(p.requests) != 1 || probe.calls.Load() != 0 {
			t.Fatalf("cancellation did not immediately stop recovery: elapsed=%s requests=%d tools=%d error=%v", elapsed, len(p.requests), probe.calls.Load(), err)
		}
	})
}

func TestLoopDefaultStreamRecoveryCustomProviderRetainsTransientRouting(t *testing.T) {
	configureDefaultStreamRecoveryTest(t)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
			if call == 1 {
				return nil, errors.New("dial tcp: connection refused")
			}
			return textStream("recovered"), nil
		}}
		if err := newStreamRecoveryTestLoop(p, probe).Run(context.Background(), make(chan Event, 128)); err != nil || len(p.requests) != 2 {
			t.Fatalf("custom provider lost its existing transient routing: requests=%d error=%v", len(p.requests), err)
		}
	})
}

func TestLoopDefaultStreamRecoveryTerminalBoundaries(t *testing.T) {
	reset := http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unknown", errors.New("unknown provider error")},
		{"cancelled", context.Canceled},
		{"exhausted", &transport.RetryExhaustedError{Err: reset, Attempts: 3}},
		{"bad_request_reset", &transport.HTTPStatusError{StatusCode: 400, Err: reset}},
		{"auth_reset", &transport.HTTPStatusError{StatusCode: 401, Err: reset}},
		{"quota", &transport.RetryableError{Err: errors.New("insufficient_quota")}},
	} {
		for _, afterHeaders := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_after_headers_%t", tc.name, afterHeaders), func(t *testing.T) {
				configureDefaultStreamRecoveryTest(t)
				probe := &recoveryTestTool{}
				p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
					if !afterHeaders {
						return nil, tc.err
					}
					cut := incompleteRecoveryToolStream(true)
					cut.err = tc.err
					return cut, nil
				}}
				err := newStreamRecoveryTestLoop(p, probe).Run(context.Background(), make(chan Event, 128))
				if !errors.Is(err, tc.err) || len(p.requests) != 1 || probe.calls.Load() != 0 {
					t.Fatalf("terminal failure retried or dispatched tools: requests=%d tools=%d error=%v", len(p.requests), probe.calls.Load(), err)
				}
			})
		}
	}
}

func newStreamRecoveryTestLoop(p *recoveryTestProvider, probe *recoveryTestTool) *Loop {
	registry := tools.NewRegistry()
	registry.Register(probe)
	loop := NewLoop(p, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 8)
	loop.AppendUser("perform the probe and report the result")
	return loop
}

func incompleteRecoveryToolStream(closedTool bool) *recoveryTestStream {
	events := []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "UNCOMMITTED_PARTIAL"},
		{Type: "tool_use_start", ToolUseID: "uncommitted-call", ToolName: "RecoveryProbe"},
		{Type: "tool_input_delta", ToolUseID: "uncommitted-call", InputDelta: `{"value":"`},
	}
	if closedTool {
		events = append(events,
			llm.StreamEvent{Type: "tool_input_delta", ToolUseID: "uncommitted-call", InputDelta: `discarded"}`},
			llm.StreamEvent{Type: "tool_use_stop", ToolUseID: "uncommitted-call"},
			llm.StreamEvent{Type: "message_delta", StopReason: "tool_use"},
		)
	}
	// Real streaming adapters classify a missing protocol terminator as
	// UnexpectedEOF, not the normal EOF returned after message_stop.
	return &recoveryTestStream{events: events, err: io.ErrUnexpectedEOF}
}

func assertNoUncommittedRecoveryHistory(t *testing.T, history []llm.Message) {
	t.Helper()
	data, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"UNCOMMITTED_PARTIAL", "uncommitted-call", "metis.partial"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("uncommitted response %q leaked into history: %s", forbidden, data)
		}
	}
}

func TestLoopStreamRecoveryIncompleteToolRunsOnlyAfterCompletedRetry(t *testing.T) {
	for _, closedTool := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed_tool_%t", closedTool), func(t *testing.T) {
			configureStreamRecoveryTest(t, true)
			synctest.Test(t, func(t *testing.T) {
				probe := &recoveryTestTool{}
				cut := incompleteRecoveryToolStream(closedTool)
				p := &recoveryTestProvider{t: t}
				p.stream = func(_ context.Context, call int) (llm.StreamReader, error) {
					switch call {
					case 1:
						return cut, nil
					case 2:
						if n := probe.calls.Load(); n != 0 {
							t.Fatalf("incomplete response already executed %d tool calls", n)
						}
						if !cut.closed {
							t.Fatal("retry started before truncated reader was closed")
						}
						return toolUseStream("accepted-call", "RecoveryProbe", `{"value":"accepted"}`), nil
					case 3:
						return textStream("probe finished"), nil
					default:
						return nil, errors.New("unexpected extra request")
					}
				}
				loop := newStreamRecoveryTestLoop(p, probe)
				if err := loop.Run(context.Background(), make(chan Event, 128)); err != nil {
					t.Fatalf("Run: %v", err)
				}
				if got := probe.calls.Load(); got != 1 {
					t.Fatalf("tool executions = %d, want exactly one completed response", got)
				}
				if len(p.requests) != 3 {
					t.Fatalf("provider calls = %d, want truncated + recovered + final", len(p.requests))
				}
				if !reflect.DeepEqual(p.requests[0], p.requests[1]) {
					t.Fatal("retry changed the original logical request")
				}
				assertNoUncommittedRecoveryHistory(t, loop.History())
			})
		})
	}
}

func TestLoopStreamRecoveryDoesNotReexecutePreviousTurnTool(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t}
		p.stream = func(_ context.Context, call int) (llm.StreamReader, error) {
			switch call {
			case 1:
				return toolUseStream("already-executed", "RecoveryProbe", `{"value":"first"}`), nil
			case 2:
				return incompleteRecoveryToolStream(true), nil
			case 3:
				if got := probe.calls.Load(); got != 1 {
					t.Fatalf("before resumed response tool executions = %d, want one", got)
				}
				return textStream("previous result acknowledged"), nil
			default:
				return nil, errors.New("unexpected extra request")
			}
		}
		loop := newStreamRecoveryTestLoop(p, probe)
		if err := loop.Run(context.Background(), make(chan Event, 128)); err != nil {
			t.Fatal(err)
		}
		if got := probe.calls.Load(); got != 1 {
			t.Fatalf("completed previous turn was reexecuted: %d calls", got)
		}
		if len(p.requests) != 3 || !reflect.DeepEqual(p.requests[1], p.requests[2]) {
			t.Fatalf("next-turn retry did not preserve its request: %d requests", len(p.requests))
		}
		var results int
		for _, msg := range p.requests[2].Messages {
			for _, block := range msg.Content {
				if block.Type == "tool_result" && block.ToolUseID == "already-executed" && block.ToolResult == "RECORDED_ONCE" {
					results++
				}
			}
		}
		if results != 1 {
			t.Fatalf("retry contains %d prior tool results, want exactly one", results)
		}
		assertNoUncommittedRecoveryHistory(t, loop.History())
	})
}

func TestLoopStreamRecoveryDisabledKeepsSingleStreamAttempt(t *testing.T) {
	configureStreamRecoveryTest(t, false)
	probe := &recoveryTestTool{}
	p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
		return incompleteRecoveryToolStream(true), nil
	}}
	loop := newStreamRecoveryTestLoop(p, probe)
	if err := loop.Run(context.Background(), make(chan Event, 64)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Run error = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(p.requests) != 1 || probe.calls.Load() != 0 {
		t.Fatalf("disabled recovery changed behavior: requests=%d tools=%d", len(p.requests), probe.calls.Load())
	}
}

func TestLoopStreamRecoveryDoesNotRetryPermanentErrorsOrCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{name: "invalid_request", err: &transport.HTTPStatusError{StatusCode: 400, Err: errors.New("invalid request")}},
		{name: "auth", err: &transport.HTTPStatusError{StatusCode: 401, Err: errors.New("unauthorized")}},
		{name: "quota", err: &transport.RetryableError{Err: errors.New("insufficient_quota")}},
		{name: "parent_cancel", err: io.ErrUnexpectedEOF, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureStreamRecoveryTest(t, true)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				probe := &recoveryTestTool{}
				p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
					stream := incompleteRecoveryToolStream(true)
					stream.err = tc.err
					if tc.cancel {
						stream.onError = cancel
					}
					return stream, nil
				}}
				loop := newStreamRecoveryTestLoop(p, probe)
				err := loop.Run(ctx, make(chan Event, 64))
				want := tc.err
				if tc.cancel {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("Run error = %v, want %v", err, want)
				}
				if len(p.requests) != 1 || probe.calls.Load() != 0 {
					t.Fatalf("terminal failure retried or executed: requests=%d tools=%d", len(p.requests), probe.calls.Load())
				}
			})
		})
	}
}

func TestLoopStreamRecoverySuccessfulHandshakesCannotResetAttemptBudget(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		var streams []*recoveryTestStream
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			stream := incompleteRecoveryToolStream(true)
			streams = append(streams, stream)
			return stream, nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		// Defensive virtual timeout turns a reset/infinite-loop regression into
		// a bounded test failure. The real configured allowance is ten minutes.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := loop.Run(ctx, make(chan Event, 4096))
		var exhausted *transport.RetryExhaustedError
		if !errors.As(err, &exhausted) || exhausted.Attempts != 3 || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Run error = %v, want shared three-attempt exhaustion preserving EOF", err)
		}
		if len(p.requests) != 3 || probe.calls.Load() != 0 {
			t.Fatalf("200 + truncated streams reset budget or dispatched tools: requests=%d tools=%d", len(p.requests), probe.calls.Load())
		}
		for i, stream := range streams {
			if !stream.closed {
				t.Errorf("attempt %d reader was not closed", i+1)
			}
		}
		for _, req := range p.requests[1:] {
			if !reflect.DeepEqual(p.requests[0], req) {
				t.Error("bounded recovery changed the original request")
			}
		}
		// Only the final failed prose may be kept for audit. No interrupted
		// response may contribute executable calls or multiple partial entries.
		partialMessages := 0
		for _, message := range loop.History() {
			partial := false
			for _, block := range message.Content {
				if block.Type == "tool_use" || block.Type == "tool_result" {
					t.Fatalf("failed recovery persisted an executable call: %+v", block)
				}
				partial = partial || block.ProviderHint["metis.partial"] == "true"
			}
			if partial {
				partialMessages++
			}
		}
		if partialMessages > 1 {
			t.Fatalf("intermediate failures were committed: %d partial messages", partialMessages)
		}
	})
}

func TestLoopStreamRecoveryCancellationInterruptsBackoff(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			return incompleteRecoveryToolStream(true), nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		started := time.Now()
		err := loop.Run(ctx, make(chan Event, 128))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation during retry delay", err)
		}
		if elapsed := time.Since(started); elapsed != 100*time.Millisecond {
			t.Fatalf("cancellation latency = %s, want immediate cancellation at 100 ms", elapsed)
		}
		if len(p.requests) != 1 || probe.calls.Load() != 0 {
			t.Fatalf("work continued after backoff cancellation: requests=%d tools=%d", len(p.requests), probe.calls.Load())
		}
	})
}

func TestLoopStreamRecoveryHTTPAndStreamFailuresShareOneBudget(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
			if call == 2 {
				return incompleteRecoveryToolStream(true), nil
			}
			return nil, transport.ErrNetwork
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := loop.Run(ctx, make(chan Event, 4096))
		var exhausted *transport.RetryExhaustedError
		if !errors.As(err, &exhausted) || exhausted.Attempts != 3 || !errors.Is(err, transport.ErrNetwork) {
			t.Fatalf("Run error = %v, want three total dial/SSE attempts", err)
		}
		if len(p.requests) != 3 || probe.calls.Load() != 0 {
			t.Fatalf("HTTP and stream failures got separate budgets: requests=%d tools=%d", len(p.requests), probe.calls.Load())
		}
		for _, req := range p.requests[1:] {
			if !reflect.DeepEqual(p.requests[0], req) {
				t.Fatal("HTTP/stream retry changed the logical request")
			}
		}
	})
}

func TestLoopStreamRecoveryCompleteResponseStartsFreshLogicalBudget(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
			switch call {
			case 1, 2, 4, 5:
				return incompleteRecoveryToolStream(true), nil
			case 3:
				return toolUseStream("one-effect", "RecoveryProbe", `{"value":"accepted"}`), nil
			case 6:
				return textStream("complete"), nil
			default:
				return nil, errors.New("unexpected extra request")
			}
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		if err := loop.Run(context.Background(), make(chan Event, 256)); err != nil {
			t.Fatal(err)
		}
		if len(p.requests) != 6 || probe.calls.Load() != 1 {
			t.Fatalf("completed response did not get fresh budget: requests=%d tools=%d", len(p.requests), probe.calls.Load())
		}
		for _, group := range [][]llm.Request{p.requests[:3], p.requests[3:]} {
			if !reflect.DeepEqual(group[0], group[1]) || !reflect.DeepEqual(group[1], group[2]) {
				t.Fatal("attempts within a logical response did not preserve the request")
			}
		}
		if reflect.DeepEqual(p.requests[0], p.requests[3]) {
			t.Fatal("second logical response lost the accepted tool result")
		}
		assertNoUncommittedRecoveryHistory(t, loop.History())
	})
}

func TestLoopStreamRecoveryExhaustionPersistsOnlyProse(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "1")
	probe := &recoveryTestTool{}
	cut := incompleteRecoveryToolStream(true)
	cut.events = append([]llm.StreamEvent{
		{Type: "thinking_delta", TextDelta: "uncommitted reasoning"},
		{Type: "thinking_signature", ProviderHint: map[string]string{"signature": "incomplete-signature"}},
		{Type: "redacted_thinking", TextDelta: "incomplete-ciphertext", ProviderHint: map[string]string{"id": "incomplete-reasoning-id"}},
		{Type: "provider_state", ProviderHint: map[string]string{"previous_response_id": "incomplete-response-with-unexecuted-call"}},
	}, cut.events...)
	p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
		if call == 1 {
			return cut, nil
		}
		return textStream("resumed safely"), nil
	}}
	loop := newStreamRecoveryTestLoop(p, probe)
	err := loop.Run(context.Background(), make(chan Event, 128))
	if !transport.IsRetryExhausted(err) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Run error = %v, want first-attempt recovery exhaustion", err)
	}
	if probe.calls.Load() != 0 {
		t.Fatal("exhausted draft executed a tool")
	}
	var partialProse bool
	for _, message := range loop.History() {
		if message.Role != llm.RoleAssistant {
			continue
		}
		for _, block := range message.Content {
			if block.Type != "text" || block.Data != "" {
				t.Fatalf("failed draft retained replayable provider state: %+v", block)
			}
			partialProse = partialProse || block.Text == "UNCOMMITTED_PARTIAL" && block.ProviderHint["metis.partial"] == "true"
		}
	}
	if !partialProse {
		t.Fatal("terminal failure must preserve visible prose marked partial for audit")
	}
	loop.AppendUser("resume from the last safe checkpoint")
	if err := loop.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 || probe.calls.Load() != 0 {
		t.Fatalf("resume replayed abandoned tool effects: requests=%d tools=%d", len(p.requests), probe.calls.Load())
	}
	for _, message := range p.requests[1].Messages {
		for _, block := range message.Content {
			if block.Type != "text" {
				t.Fatalf("resume sent abandoned provider state or tool call: %+v", block)
			}
		}
	}
}

func TestLoopStreamRecoveryResponsesWireSharesHTTPAndSSEBudget(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		call := len(bodies)
		mu.Unlock()
		switch call {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary unavailable"}}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"UNCOMMITTED_PARTIAL"}

data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"incomplete-reasoning-id","encrypted_content":"incomplete-ciphertext","summary":[]}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"incomplete-item","call_id":"uncommitted-call","name":"RecoveryProbe"}}

data: {"type":"response.function_call_arguments.delta","item_id":"incomplete-item","delta":"{\"value\":\"discarded\"}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"incomplete-item","call_id":"uncommitted-call","name":"RecoveryProbe","arguments":"{\"value\":\"discarded\"}"}}

`)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"recovered complete response"}

data: {"type":"response.completed","response":{"id":"complete-response","status":"completed","output":[]}}

`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected extra POST"}}`)
		}
	}))
	t.Cleanup(server.Close)
	provider := openai.NewResponses("test-key", server.URL, "gpt-test", 256, 5*time.Second, 0)
	probe := &recoveryTestTool{}
	registry := tools.NewRegistry()
	registry.Register(probe)
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 8)
	loop.AppendUser("perform the probe only if the provider completes its tool call")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := loop.Run(ctx, make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	captured := append([]string(nil), bodies...)
	mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("actual HTTP POSTs = %d, want exactly 503 + truncated 200 + complete 200", len(captured))
	}
	if captured[0] != captured[1] || captured[1] != captured[2] {
		t.Fatal("actual retry payload changed or included uncommitted provider state")
	}
	if probe.calls.Load() != 0 {
		t.Fatal("truncated real Responses tool call was executed")
	}
	assertNoUncommittedRecoveryHistory(t, loop.History())
	if !historyContainsText(loop.History(), "recovered complete response") {
		t.Fatal("complete real Responses answer was not persisted")
	}
}

func testRecoveryBackpressure(t *testing.T, streamFailure bool) {
	t.Helper()
	configureStreamRecoveryTest(t, true)
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "1")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out := make(chan Event, 1)
		out <- Event{Kind: EventInfo, Info: "consumer currently backpressured"}
		recoveryCtx, session, err := newRequestRecovery(ctx, out)
		if err != nil {
			t.Fatal(err)
		}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			if streamFailure {
				// No content deltas: the only generated events are optional
				// recovery progress/discard notices, not required model output.
				return &recoveryTestStream{err: io.ErrUnexpectedEOF}, nil
			}
			return nil, transport.ErrNetwork
		}}
		started := time.Now()
		stream, err := openStreamWithRecovery(recoveryCtx, p, llm.Request{}, session)
		if streamFailure && err == nil {
			_, _, _, err = (&Loop{}).consumeStreamWithRecovery(recoveryCtx, p, llm.Request{}, stream, out, session)
		}
		if elapsed := time.Since(started); elapsed != time.Second {
			t.Fatalf("optional recovery notices extended the budget: elapsed=%s err=%v", elapsed, err)
		}
		if !errors.Is(err, transport.ErrRecoveryWindowExceeded) || !transport.IsRetryExhausted(err) {
			t.Fatalf("error=%v, want recovery-window exhaustion rather than parent cancellation", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("recovery waited until parent deadline: %v", ctx.Err())
		}
		if len(p.requests) != 2 {
			t.Fatalf("requests=%d, want two attempts within the one-second window", len(p.requests))
		}
		if event := <-out; event.Info != "consumer currently backpressured" {
			t.Fatalf("recovery displaced an existing required event: %+v", event)
		}
	})
}

func TestLoopStreamRecoveryHTTPProgressBackpressureCannotExtendWindow(t *testing.T) {
	testRecoveryBackpressure(t, false)
}

func TestLoopStreamRecoveryDiscardNoticeBackpressureCannotExtendWindow(t *testing.T) {
	testRecoveryBackpressure(t, true)
}
