package anthropic

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAnthropicStreamRawEOFFailsClosedWithoutProtocolTerminator(t *testing.T) {
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

`)))
	t.Cleanup(func() { _ = stream.Close() })

	if event, err := stream.Recv(); err != nil || event.Type != "message_start" {
		t.Fatalf("first Recv() = (%+v, %v), want message_start", event, err)
	}
	event, err := stream.Recv()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("terminal Recv() error = %v, want io.ErrUnexpectedEOF", err)
	}
	if event.Type == "message_stop" {
		t.Fatalf("raw EOF was synthesized as a successful message_stop: %+v", event)
	}
}

func TestAnthropicStreamRawEOFWithOpenToolBlockNeverEmitsToolStop(t *testing.T) {
	body := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_cut","name":"Bash","input":{}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"touch /tmp/must-not-run"}}

`
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(body)))
	t.Cleanup(func() { _ = stream.Close() })

	var seen []string
	for {
		event, err := stream.Recv()
		if event.Type != "" {
			seen = append(seen, event.Type)
		}
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("Recv() error = %v, want io.ErrUnexpectedEOF", err)
			}
			break
		}
	}

	for _, eventType := range seen {
		if eventType == "tool_use_stop" || eventType == "message_stop" {
			t.Fatalf("truncated tool call emitted executable terminal event %q; events=%v", eventType, seen)
		}
	}
}

func TestAnthropicStreamRejectsProtocolTerminatorWithOpenToolBlock(t *testing.T) {
	body := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_cut","name":"Bash","input":{}}}

data: {"type":"message_stop"}

`
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(body)))
	t.Cleanup(func() { _ = stream.Close() })

	if event, err := stream.Recv(); err != nil || event.Type != "tool_use_start" {
		t.Fatalf("first Recv() = (%+v, %v), want tool_use_start", event, err)
	}
	event, err := stream.Recv()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("message_stop with an open tool block error = %v, want io.ErrUnexpectedEOF", err)
	}
	if event.Type == "message_stop" || event.Type == "tool_use_stop" {
		t.Fatalf("open tool block was accepted at message_stop: %+v", event)
	}
}

func TestAnthropicStreamDoneSentinelIsAProtocolTerminator(t *testing.T) {
	stream := newAnthropicStream(io.NopCloser(strings.NewReader("data: [DONE]\n\n")))
	t.Cleanup(func() { _ = stream.Close() })

	event, err := stream.Recv()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() error = %v, want io.EOF", err)
	}
	if event.Type != "message_stop" {
		t.Fatalf("Recv() event = %+v, want protocol message_stop", event)
	}
}
