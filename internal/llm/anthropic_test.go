package llm

import (
	"io"
	"strings"
	"testing"
)

func makeAnthStream(payload string) *anthropicStream {
	return newAnthropicStream(io.NopCloser(strings.NewReader(payload)))
}

func TestAnthropicStream_TextOnly(t *testing.T) {
	payload := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	s := makeAnthStream(payload)
	defer s.Close()

	expected := []struct {
		typ  string
		text string
	}{
		{"message_start", ""},
		{"text_delta", "Hello"},
		{"text_delta", " world"},
		{"message_delta", ""},
		{"message_stop", ""},
	}
	for i, e := range expected {
		ev, err := s.Recv()
		if i == len(expected)-1 {
			if err != io.EOF {
				t.Fatalf("event %d: want EOF, got %v", i, err)
			}
		} else if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if ev.Type != e.typ {
			t.Errorf("event %d: type=%q want %q", i, ev.Type, e.typ)
		}
		if ev.TextDelta != e.text {
			t.Errorf("event %d: text=%q want %q", i, ev.TextDelta, e.text)
		}
	}
}

func TestAnthropicStream_ToolUse(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"LS","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"/tmp\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	s := makeAnthStream(payload)
	defer s.Close()

	must := func(typ string) StreamEvent {
		ev, err := s.Recv()
		if err != nil && err != io.EOF {
			t.Fatalf("recv err: %v", err)
		}
		if ev.Type != typ {
			t.Fatalf("want %q got %q", typ, ev.Type)
		}
		return ev
	}
	start := must("tool_use_start")
	if start.ToolName != "LS" || start.ToolUseID != "toolu_1" {
		t.Errorf("start: name=%q id=%q", start.ToolName, start.ToolUseID)
	}
	must("tool_input_delta")
	must("tool_input_delta")
	stop := must("tool_use_stop")
	if stop.InputDelta != `{"path":"/tmp"}` {
		t.Errorf("input json = %q", stop.InputDelta)
	}
	must("message_delta")
	must("message_stop")
}
