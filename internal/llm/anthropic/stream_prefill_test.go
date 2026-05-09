package anthropic

import (
	"io"
	"strings"
	"testing"
)

// streamFromString wraps a literal SSE byte string as the response
// body for newAnthropicStream. We don't drive the HTTP layer; we just
// feed bytes that mimic the wire format the upstream gateway sends.
//
// nopCloser turns an io.Reader into io.ReadCloser without closing
// machinery; the stream's body is read once and abandoned.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func drainStream(t *testing.T, body string) []StreamEvent {
	t.Helper()
	s := newAnthropicStream(nopCloser{strings.NewReader(body)})
	defer s.Close()
	var out []StreamEvent
	for {
		ev, err := s.Recv()
		out = append(out, ev)
		if err != nil {
			return out
		}
	}
}

// TestStream_ToolUse_PrefillOnly_NoDelta covers the "prefill mode" the
// model uses for short payloads or no-arg invocations: the entire
// arguments object is carried inside content_block_start.input, and
// no input_json_delta events follow. Before this fix, the prefilled
// arguments were dropped and the agent's transcript landed with empty
// ToolInput, breaking the next request's tool_use round-trip.
func TestStream_ToolUse_PrefillOnly_NoDelta(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"MetisInfo","input":{"section":"providers"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

event: message_stop
data: {"type":"message_stop"}

`
	events := drainStream(t, body)
	var stop StreamEvent
	for _, e := range events {
		if e.Type == "tool_use_stop" {
			stop = e
			break
		}
	}
	if stop.Type != "tool_use_stop" {
		t.Fatalf("no tool_use_stop event in %+v", events)
	}
	if !strings.Contains(stop.InputDelta, `"section":"providers"`) {
		t.Errorf("prefill not preserved at tool_use_stop: %q", stop.InputDelta)
	}
}

// TestStream_ToolUse_DeltaOnly_NoPrefill covers the canonical streaming
// path: content_block_start carries only metadata (empty input), then
// input_json_delta events accumulate the JSON. This is the path that
// has worked since day one — verify it still does.
func TestStream_ToolUse_DeltaOnly_NoPrefill(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_2","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	events := drainStream(t, body)
	var stop StreamEvent
	for _, e := range events {
		if e.Type == "tool_use_stop" {
			stop = e
			break
		}
	}
	if stop.Type != "tool_use_stop" {
		t.Fatalf("no tool_use_stop event")
	}
	if stop.InputDelta != `{"command":"ls"}` {
		t.Errorf("delta path broken: %q", stop.InputDelta)
	}
}

// TestStream_ToolUse_BothPrefillAndDelta verifies the hybrid case:
// some gateways may send a partial prefill then complete it via
// deltas. The deltas are the canonical streaming form and carry the
// full final JSON, so we MUST prefer them — concatenating prefill +
// delta would produce garbage. The fix uses Prefill only when JSONBuf
// is empty.
func TestStream_ToolUse_BothPrefillAndDelta(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_3","name":"Bash","input":{"will_be":"replaced"}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo hi\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	events := drainStream(t, body)
	var stop StreamEvent
	for _, e := range events {
		if e.Type == "tool_use_stop" {
			stop = e
			break
		}
	}
	if stop.Type != "tool_use_stop" {
		t.Fatalf("no tool_use_stop event")
	}
	// Deltas should win when present — they're the streaming form of
	// the final canonical JSON.
	if stop.InputDelta != `{"command":"echo hi"}` {
		t.Errorf("expected delta to win over prefill, got %q", stop.InputDelta)
	}
}

// TestStream_ToolUse_EmptyPrefillNoDelta covers the truly-empty case:
// content_block_start.input is `{}`, no deltas. Result should be an
// empty InputDelta string — the MarshalJSON fix on anthropicContent
// (separate file, separate concern) ensures it round-trips as
// `"input":{}` on the next request, so MiniMax 2013 doesn't fire.
func TestStream_ToolUse_EmptyPrefillNoDelta(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_4","name":"MetisInfo","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	events := drainStream(t, body)
	var stop StreamEvent
	for _, e := range events {
		if e.Type == "tool_use_stop" {
			stop = e
			break
		}
	}
	if stop.Type != "tool_use_stop" {
		t.Fatalf("no tool_use_stop event")
	}
	if stop.InputDelta != "" {
		t.Errorf("expected empty InputDelta for empty {} prefill, got %q", stop.InputDelta)
	}
}

// TestStream_ToolUse_NoInputFieldAtAll covers the wildest case: gateway
// omits the `input` field entirely from content_block_start (rare but
// observed in some compat servers). cb.Input unmarshals to nil; we
// must not crash and must produce a clean empty InputDelta.
func TestStream_ToolUse_NoInputFieldAtAll(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_5","name":"MetisInfo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	events := drainStream(t, body)
	var stop StreamEvent
	for _, e := range events {
		if e.Type == "tool_use_stop" {
			stop = e
			break
		}
	}
	if stop.Type != "tool_use_stop" {
		t.Fatalf("no tool_use_stop event")
	}
	if stop.InputDelta != "" {
		t.Errorf("expected empty InputDelta when input field is absent, got %q", stop.InputDelta)
	}
}
