package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestConsumeStreamLatestAbsoluteUsageWithoutPresenceFlags(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: 556},
		{Type: "message_delta", InputTokens: 179, OutputTokens: 74, CacheCreationInputTokens: 512},
		{Type: "message_stop"},
	}}
	_, _, usage, err := (&Loop{}).consumeStream(context.Background(), stream, make(chan Event, 4))
	if err != nil {
		t.Fatal(err)
	}
	want := usageTotals{in: 179, out: 74, cacheCreate: 512}
	if usage == nil || *usage != want {
		t.Fatalf("latest absolute usage = %+v, want %+v", usage, want)
	}
}

func TestConsumeStreamAbsoluteUsageFieldPresence(t *testing.T) {
	initial := llm.StreamEvent{Type: "message_start", InputTokens: 556, OutputTokens: 100, CacheCreationInputTokens: 600, CacheReadInputTokens: 10}
	tests := []struct {
		name   string
		update llm.StreamEvent
		want   usageTotals
	}{
		{"input zero", llm.StreamEvent{HasInputTokens: true}, usageTotals{out: 100, cacheCreate: 600, cacheRead: 10}},
		{"output zero", llm.StreamEvent{HasOutputTokens: true}, usageTotals{in: 556, cacheCreate: 600, cacheRead: 10}},
		{"cache creation zero", llm.StreamEvent{HasCacheCreationInputTokens: true}, usageTotals{in: 556, out: 100, cacheRead: 10}},
		{"cache read zero", llm.StreamEvent{HasCacheReadInputTokens: true}, usageTotals{in: 556, out: 100, cacheCreate: 600}},
		{"missing fields", llm.StreamEvent{}, usageTotals{in: 556, out: 100, cacheCreate: 600, cacheRead: 10}},
		{"legacy downward correction", llm.StreamEvent{InputTokens: 179, OutputTokens: 74, CacheCreationInputTokens: 512, CacheReadInputTokens: 9}, usageTotals{in: 179, out: 74, cacheCreate: 512, cacheRead: 9}},
		{"present downward correction", llm.StreamEvent{InputTokens: 179, HasInputTokens: true, OutputTokens: 74, HasOutputTokens: true, CacheCreationInputTokens: 512, HasCacheCreationInputTokens: true, CacheReadInputTokens: 9, HasCacheReadInputTokens: true}, usageTotals{in: 179, out: 74, cacheCreate: 512, cacheRead: 9}},
		{"legacy negative ignored", llm.StreamEvent{InputTokens: -1, OutputTokens: -1, CacheCreationInputTokens: -1, CacheReadInputTokens: -1}, usageTotals{in: 556, out: 100, cacheCreate: 600, cacheRead: 10}},
	}
	for _, eventType := range []string{"message_start", "message_delta", "usage", "message_stop"} {
		for _, tt := range tests {
			t.Run(eventType+"/"+tt.name, func(t *testing.T) {
				update := tt.update
				update.Type = eventType
				stream := &mockStream{events: []llm.StreamEvent{initial, update, {Type: "message_stop"}}}
				_, _, usage, err := (&Loop{}).consumeStream(context.Background(), stream, make(chan Event, 4))
				if err != nil || usage == nil || *usage != tt.want {
					t.Fatalf("usage=%+v error=%v, want %+v", usage, err, tt.want)
				}
			})
		}
	}
}

func TestConsumeStreamNegativePresentUsagePreservesPreviousSnapshot(t *testing.T) {
	initial := llm.StreamEvent{Type: "message_start", InputTokens: 556, OutputTokens: 100, CacheCreationInputTokens: 600, CacheReadInputTokens: 10}
	badUpdates := []llm.StreamEvent{
		{InputTokens: -1, HasInputTokens: true, HasOutputTokens: true},
		{OutputTokens: -1, HasOutputTokens: true, HasInputTokens: true},
		{CacheCreationInputTokens: -1, HasCacheCreationInputTokens: true, HasInputTokens: true},
		{CacheReadInputTokens: -1, HasCacheReadInputTokens: true, HasInputTokens: true},
	}
	for _, eventType := range []string{"message_start", "message_delta", "usage", "message_stop"} {
		for i, update := range badUpdates {
			update.Type = eventType
			stream := &mockStream{events: []llm.StreamEvent{initial, update}}
			_, _, usage, err := (&Loop{}).consumeStream(context.Background(), stream, make(chan Event, 4))
			want := usageTotals{in: 556, out: 100, cacheCreate: 600, cacheRead: 10}
			if err == nil || !strings.Contains(err.Error(), "negative") || usage == nil || *usage != want {
				t.Fatalf("%s negative field %d: usage=%+v error=%v, want preserved %+v", eventType, i, usage, err, want)
			}
		}
	}
}

type usageValueAndErrorStream struct {
	calls int
}

func (s *usageValueAndErrorStream) Recv() (llm.StreamEvent, error) {
	s.calls++
	if s.calls == 1 {
		return llm.StreamEvent{Type: "message_start", InputTokens: 556}, nil
	}
	return llm.StreamEvent{Type: "usage", InputTokens: 179, HasInputTokens: true, OutputTokens: 74, HasOutputTokens: true, CacheCreationInputTokens: 512, HasCacheCreationInputTokens: true}, io.ErrUnexpectedEOF
}

func (*usageValueAndErrorStream) Close() error { return nil }

func TestConsumeStreamKeepsUsageReturnedWithReadError(t *testing.T) {
	_, _, usage, err := (&Loop{}).consumeStream(context.Background(), &usageValueAndErrorStream{}, make(chan Event, 4))
	want := usageTotals{in: 179, out: 74, cacheCreate: 512}
	if !errors.Is(err, io.ErrUnexpectedEOF) || usage == nil || *usage != want {
		t.Fatalf("usage=%+v error=%v, want %+v and io.ErrUnexpectedEOF", usage, err, want)
	}
}
