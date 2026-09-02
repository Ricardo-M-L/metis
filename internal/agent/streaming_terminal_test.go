package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
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

type providerErrorEventStream struct {
	step int
}

func (s *providerErrorEventStream) Recv() (llm.StreamEvent, error) {
	s.step++
	switch s.step {
	case 1:
		return llm.StreamEvent{Type: "text_delta", TextDelta: "partial answer"}, nil
	case 2:
		return llm.StreamEvent{Type: "error", Err: errors.New("provider failed")}, nil
	default:
		return llm.StreamEvent{}, io.EOF
	}
}

func (*providerErrorEventStream) Close() error { return nil }

func TestConsumeStreamReturnsProviderErrorEvent(t *testing.T) {
	blocks, _, _, err := (&Loop{}).consumeStream(
		context.Background(),
		&providerErrorEventStream{},
		make(chan Event, 2),
	)
	if err == nil || err.Error() != "provider failed" {
		t.Fatalf("consumeStream error = %v, want provider failed", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "partial answer" {
		t.Fatalf("blocks = %#v, want preserved partial text", blocks)
	}
}

type partialErrorProvider struct{}

func (partialErrorProvider) Name() string          { return "partial-error" }
func (partialErrorProvider) ModelID() string       { return "partial-error-model" }
func (partialErrorProvider) MaxContextTokens() int { return 100_000 }
func (partialErrorProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("Complete should not be called")
}
func (partialErrorProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &providerErrorEventStream{}, nil
}

func TestLoopPersistsPartialAssistantAfterStreamFailure(t *testing.T) {
	loop := NewLoop(
		partialErrorProvider{},
		tools.NewRegistry(),
		permission.New(permission.ModeBypassPermissions),
		nil,
		"system",
		2,
	)
	loop.AppendUser("hello")
	err := loop.Run(context.Background(), make(chan Event, 16))
	if err == nil || err.Error() != "provider failed" {
		t.Fatalf("Run error = %v", err)
	}
	history := loop.History()
	if len(history) != 2 || history[1].Role != llm.RoleAssistant || len(history[1].Content) != 1 {
		t.Fatalf("history = %#v", history)
	}
	partial := history[1].Content[0]
	if partial.Text != "partial answer" || partial.ProviderHint["metis.partial"] != "true" {
		t.Fatalf("partial block = %#v", partial)
	}
}

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
