package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// mockStream replays a fixed sequence of StreamEvents. Returns io.EOF
// once exhausted so consumeStream's terminal branch fires.
type mockStream struct {
	events []llm.StreamEvent
	idx    int
}

func (m *mockStream) Recv() (llm.StreamEvent, error) {
	if m.idx >= len(m.events) {
		return llm.StreamEvent{}, io.EOF
	}
	e := m.events[m.idx]
	m.idx++
	return e, nil
}

func (m *mockStream) Close() error { return nil }

// TestConsumeStream_EmitsToolArgsDelta — driving consumeStream with
// tool_input_delta wire events should now produce per-chunk
// EventToolArgsDelta on the out channel (T12). Before this change the
// chunks were silently accumulated until tool_use_stop; the UI saw
// the args only after the full tool_use block closed.
func TestConsumeStream_EmitsToolArgsDelta(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: 100},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `{"path":`},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `"/tmp/foo`},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `.go"}`},
		{Type: "tool_use_stop", ToolUseID: "t1"},
		{Type: "message_stop"},
	}}

	out := make(chan Event, 32)
	loop := &Loop{}
	go func() {
		_, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
	}()

	var (
		argsDeltas    []string
		argsDeltaTool string
	)
	for ev := range out {
		if ev.Kind == EventToolArgsDelta {
			argsDeltas = append(argsDeltas, ev.TextDelta)
			argsDeltaTool = ev.ToolName
		}
	}

	if len(argsDeltas) != 3 {
		t.Errorf("expected 3 EventToolArgsDelta events; got %d (%v)",
			len(argsDeltas), argsDeltas)
	}
	if joined := strings.Join(argsDeltas, ""); joined != `{"path":"/tmp/foo.go"}` {
		t.Errorf("concatenated deltas should reassemble the full JSON; got %q", joined)
	}
	if argsDeltaTool != "Read" {
		t.Errorf("EventToolArgsDelta should carry ToolName; got %q", argsDeltaTool)
	}
}

// TestConsumeStream_NoArgsDeltaWithoutToolStart — stray
// tool_input_delta before any tool_use_start should NOT emit an args
// event (no in-flight tool to attribute it to). Defensive — guards
// against malformed provider streams crashing the loop.
func TestConsumeStream_NoArgsDeltaWithoutToolStart(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: 1},
		{Type: "tool_input_delta", ToolUseID: "orphan", InputDelta: `{"x":1}`},
		{Type: "message_stop"},
	}}

	out := make(chan Event, 8)
	loop := &Loop{}
	go func() {
		_, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
	}()

	for ev := range out {
		if ev.Kind == EventToolArgsDelta {
			t.Errorf("orphan tool_input_delta must NOT emit EventToolArgsDelta; got %+v", ev)
		}
	}
}
