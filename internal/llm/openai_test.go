package llm

import (
	"io"
	"strings"
	"testing"
)

// makeStream wraps a raw SSE payload in a stream reader for testing.
func makeStream(payload string) *openAIStream {
	return newOpenAIStream(io.NopCloser(strings.NewReader(payload)))
}

func TestOpenAIStream_PlainText(t *testing.T) {
	// Two-chunk standard OpenAI streaming response.
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello","role":"assistant"},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world"},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	want := []struct {
		typ  string
		text string
		stop string
	}{
		{"text_delta", "Hello", ""},
		{"text_delta", " world", ""},
		{"message_delta", "", "end_turn"},
		{"message_stop", "", ""},
	}
	for i, w := range want {
		ev, err := s.Recv()
		if i == len(want)-1 {
			if err != io.EOF {
				t.Fatalf("event %d: want EOF, got err=%v", i, err)
			}
		} else if err != nil {
			t.Fatalf("event %d: unexpected err %v", i, err)
		}
		if ev.Type != w.typ {
			t.Errorf("event %d: type=%q want %q", i, ev.Type, w.typ)
		}
		if ev.TextDelta != w.text {
			t.Errorf("event %d: text=%q want %q", i, ev.TextDelta, w.text)
		}
		if ev.StopReason != w.stop {
			t.Errorf("event %d: stop=%q want %q", i, ev.StopReason, w.stop)
		}
	}
}

func TestOpenAIStream_GeminiBundledChunk(t *testing.T) {
	// Reproduces the bug fixed earlier: Gemini's OpenAI-compat layer
	// puts content + finish_reason + usage in a single chunk.
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"pong","role":"assistant"},"finish_reason":"stop","index":0}],"usage":{"completion_tokens":1,"prompt_tokens":7,"total_tokens":8}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	// Expect: text_delta "pong", then message_delta with stop+usage, then message_stop.
	ev, err := s.Recv()
	if err != nil || ev.Type != "text_delta" || ev.TextDelta != "pong" {
		t.Fatalf("event 1: want text_delta pong, got %+v err=%v", ev, err)
	}
	ev, err = s.Recv()
	if err != nil || ev.Type != "message_delta" || ev.StopReason != "end_turn" {
		t.Fatalf("event 2: want message_delta end_turn, got %+v err=%v", ev, err)
	}
	if ev.OutputTokens != 1 || ev.InputTokens != 7 {
		t.Errorf("event 2: want tokens in=7 out=1, got in=%d out=%d", ev.InputTokens, ev.OutputTokens)
	}
	ev, err = s.Recv()
	if err != io.EOF || ev.Type != "message_stop" {
		t.Fatalf("event 3: want message_stop EOF, got %+v err=%v", ev, err)
	}
}

func TestOpenAIStream_ToolCall(t *testing.T) {
	// A single tool call streamed across multiple chunks.
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"LS"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	mustNext := func(typ string) StreamEvent {
		ev, err := s.Recv()
		if err != nil && err != io.EOF {
			t.Fatalf("recv err: %v", err)
		}
		if ev.Type != typ {
			t.Fatalf("want %q got %q", typ, ev.Type)
		}
		return ev
	}
	mustNext("tool_use_start")
	mustNext("tool_input_delta")
	mustNext("tool_input_delta")
	stop := mustNext("tool_use_stop")
	if stop.InputDelta != `{"path":"/tmp"}` {
		t.Errorf("accumulated input json = %q, want %q", stop.InputDelta, `{"path":"/tmp"}`)
	}
	mustNext("message_delta")
	mustNext("message_stop")
}

// Regression: when the upstream omits `index` (e.g. some Groq/Together
// streams), the previous code defaulted every parallel call to index 0
// and merged their arguments. We now route each call to its own slot
// using the call id.
func TestOpenAIStream_ParallelToolCallsWithoutIndex(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_a","function":{"name":"LS"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"Glob"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":"{\"a\":1}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"arguments":"{\"b\":2}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	stops := map[string]string{}
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == "tool_use_stop" {
			stops[ev.ToolUseID] = ev.InputDelta
		}
	}
	if got := stops["call_a"]; got != `{"a":1}` {
		t.Errorf("call_a accumulated = %q, want {\"a\":1}", got)
	}
	if got := stops["call_b"]; got != `{"b":2}` {
		t.Errorf("call_b accumulated = %q, want {\"b\":2}", got)
	}
}
