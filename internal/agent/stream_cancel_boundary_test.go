package agent

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// A provider can finish successfully at the exact cancellation boundary. Its
// final value (including usage) remains valid even when returned with EOF.
type cancelBoundaryStream struct {
	events  []llm.StreamEvent
	final   llm.StreamEvent
	cancel  context.CancelFunc
	onClose bool
	closed  bool
}

func (s *cancelBoundaryStream) Recv() (llm.StreamEvent, error) {
	if len(s.events) > 0 {
		event := s.events[0]
		s.events = s.events[1:]
		return event, nil
	}
	if !s.onClose {
		s.cancel()
	}
	return s.final, io.EOF
}

func (s *cancelBoundaryStream) Close() error {
	s.closed = true
	if s.onClose {
		s.cancel()
	}
	return nil
}

func TestStreamCancellationAtSuccessfulTerminalRetainsUsageAndOnlyPartialText(t *testing.T) {
	for _, boundary := range []string{"eof", "message_stop", "close", "accounting"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &cancelBoundaryStream{
				cancel:  cancel,
				onClose: boundary == "close",
				events: []llm.StreamEvent{
					{Type: "text_delta", TextDelta: "VISIBLE_PARTIAL"},
					{Type: "provider_state", ProviderHint: map[string]string{"response_id": "unaccepted-response"}},
					{Type: "tool_use_start", ToolUseID: "unaccepted-call", ToolName: "RecoveryProbe"},
					{Type: "tool_use_stop", ToolUseID: "unaccepted-call", InputDelta: `{"value":"must-not-run"}`},
				},
				final: llm.StreamEvent{InputTokens: 41, OutputTokens: 7, HasInputTokens: true, HasOutputTokens: true},
			}
			if boundary != "eof" {
				stream.final.Type = "message_stop"
				stream.final.StopReason = "tool_use"
			}
			if boundary == "accounting" {
				stream.cancel = func() {}
			}
			loop := &Loop{}
			var observed usageTotals
			calls := 0
			ledgerErr := errors.New("durable usage fixture failure")
			account := func(usage *usageTotals) error {
				calls++
				observed = *usage
				if boundary == "accounting" {
					cancel()
					return ledgerErr
				}
				return nil
			}
			blocks, _, usage, err := loop.consumeStreamWithRecoveryUsage(ctx, nil, llm.Request{}, stream, make(chan Event, 128), nil, account)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("successful terminal hid cancellation: %v", err)
			}
			if boundary == "accounting" && !errors.Is(err, ledgerErr) {
				t.Fatal("cancellation replaced the durable accounting failure")
			}
			if !stream.closed || calls != 1 || usage == nil || usage.in != 41 || usage.out != 7 || observed != *usage {
				t.Fatalf("lost terminal usage or stream ownership: closed=%t calls=%d usage=%+v observed=%+v", stream.closed, calls, usage, observed)
			}
			want := []llm.ContentBlock{{Type: "text", Text: "VISIBLE_PARTIAL"}}
			if !reflect.DeepEqual(blocks, want) {
				t.Fatalf("cancelled terminal retained replayable provider state or lost partial prose: %+v", blocks)
			}
		})
	}
}

func TestLoopCancelledEmptyTerminalDoesNotAppendRescueInput(t *testing.T) {
	for _, terminal := range []string{"eof", "message_stop"} {
		t.Run(terminal, func(t *testing.T) {
			t.Setenv("METIS_RECOVERY_MAX_SECONDS", "0")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &cancelBoundaryStream{cancel: cancel}
			if terminal == "message_stop" {
				stream.final = llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}
			}
			provider := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) { return stream, nil }}
			probe := &recoveryTestTool{}
			loop := newStreamRecoveryTestLoop(provider, probe)
			before := loop.History()
			if err := loop.Run(ctx, make(chan Event, 128)); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled empty terminal returned %v", err)
			}
			if len(provider.requests) != 1 || probe.calls.Load() != 0 || !reflect.DeepEqual(before, loop.History()) {
				t.Fatalf("cancellation appended rescue input or repeated work: requests=%d tools=%d history=%+v", len(provider.requests), probe.calls.Load(), loop.History())
			}
		})
	}
}
