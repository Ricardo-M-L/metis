package agent

import (
	"context"
	"io"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// terminalEOFStream models readers that return their final event and io.EOF
// together. The event is still a valid value and must be consumed.
type terminalEOFStream struct {
	calls int
}

func (s *terminalEOFStream) Recv() (llm.StreamEvent, error) {
	s.calls++
	switch s.calls {
	case 1:
		return llm.StreamEvent{Type: "text_delta", TextDelta: "done"}, nil
	case 2:
		return llm.StreamEvent{
			Type:                     "message_stop",
			StopReason:               "end_turn",
			InputTokens:              1_000,
			OutputTokens:             50,
			CacheCreationInputTokens: 100,
			CacheReadInputTokens:     200,
		}, io.EOF
	default:
		return llm.StreamEvent{}, io.EOF
	}
}

func (*terminalEOFStream) Close() error { return nil }

func TestConsumeStreamProcessesTerminalEventBeforeEOF(t *testing.T) {
	stream := &terminalEOFStream{}
	out := make(chan Event, 8)

	blocks, stop, usage, err := (&Loop{}).consumeStream(context.Background(), stream, out)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if stream.calls != 2 {
		t.Fatalf("Recv calls = %d, want 2", stream.calls)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "done" {
		t.Fatalf("blocks = %+v, want terminal text block", blocks)
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", stop)
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.in != 1_000 || usage.out != 50 || usage.cacheCreate != 100 || usage.cacheRead != 200 {
		t.Fatalf("usage = %+v, want terminal message_stop counters", usage)
	}
}
