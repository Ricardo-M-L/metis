package anthropic

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type usagePresenceSnapshotForTest struct {
	input, output, cacheCreation, cacheRead             int
	hasInput, hasOutput, hasCacheCreation, hasCacheRead bool
}

func assertAnthropicUsagePresence(t *testing.T, event StreamEvent, want usagePresenceSnapshotForTest) {
	t.Helper()
	got := usagePresenceSnapshotForTest{
		input:            event.InputTokens,
		output:           event.OutputTokens,
		cacheCreation:    event.CacheCreationInputTokens,
		cacheRead:        event.CacheReadInputTokens,
		hasInput:         event.HasInputTokens,
		hasOutput:        event.HasOutputTokens,
		hasCacheCreation: event.HasCacheCreationInputTokens,
		hasCacheRead:     event.HasCacheReadInputTokens,
	}
	if got != want {
		t.Errorf("%s usage = %+v, want %+v", event.Type, got, want)
	}
}

func anthropicUsageFrameForTest(eventType, usage string) string {
	if eventType == "message_start" {
		return fmt.Sprintf("event: %s\ndata: {\"type\":%q,\"message\":{\"usage\":%s}}\n\n", eventType, eventType, usage)
	}
	return fmt.Sprintf("event: %s\ndata: {\"type\":%q,\"usage\":%s}\n\n", eventType, eventType, usage)
}

func recvAnthropicUsageFrameForTest(t *testing.T, eventType, usage string) StreamEvent {
	t.Helper()
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(anthropicUsageFrameForTest(eventType, usage))))
	t.Cleanup(func() { _ = stream.Close() })
	event, err := stream.Recv()
	if eventType == "message_stop" {
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Recv() error = %v, want io.EOF for message_stop", err)
		}
	} else if err != nil {
		t.Fatalf("Recv() error = %v, want nil", err)
	}
	if event.Type != eventType {
		t.Fatalf("Recv() type = %q, want %q", event.Type, eventType)
	}
	return event
}

func TestAnthropicStreamUsagePresenceFinalSnapshotMayDecreaseInput(t *testing.T) {
	// This is the native SSE shape of a gateway correcting the input bucket
	// after resolving which prompt tokens were written to cache.
	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":556,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":179,"output_tokens":74,"cache_creation_input_tokens":512}}

event: message_stop
data: {"type":"message_stop"}

`
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(body)))
	t.Cleanup(func() { _ = stream.Close() })

	start, err := stream.Recv()
	if err != nil || start.Type != "message_start" {
		t.Fatalf("first Recv() = (%+v, %v), want message_start", start, err)
	}
	assertAnthropicUsagePresence(t, start, usagePresenceSnapshotForTest{
		input: 556, hasInput: true, hasOutput: true,
	})

	delta, err := stream.Recv()
	if err != nil || delta.Type != "message_delta" {
		t.Fatalf("second Recv() = (%+v, %v), want message_delta", delta, err)
	}
	assertAnthropicUsagePresence(t, delta, usagePresenceSnapshotForTest{
		input: 179, output: 74, cacheCreation: 512,
		hasInput: true, hasOutput: true, hasCacheCreation: true,
	})
	if delta.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", delta.StopReason)
	}

	stop, err := stream.Recv()
	if !errors.Is(err, io.EOF) || stop.Type != "message_stop" {
		t.Fatalf("terminal Recv() = (%+v, %v), want message_stop with io.EOF", stop, err)
	}
	// A metadata-free stop must not repeat earlier snapshot values or flags.
	assertAnthropicUsagePresence(t, stop, usagePresenceSnapshotForTest{})
}

func TestAnthropicStreamUsagePresenceZeroMissingAndNull(t *testing.T) {
	fields := []struct {
		name string
		want usagePresenceSnapshotForTest
	}{
		{"input_tokens", usagePresenceSnapshotForTest{hasInput: true}},
		{"output_tokens", usagePresenceSnapshotForTest{hasOutput: true}},
		{"cache_creation_input_tokens", usagePresenceSnapshotForTest{hasCacheCreation: true}},
		{"cache_read_input_tokens", usagePresenceSnapshotForTest{hasCacheRead: true}},
	}
	for _, eventType := range []string{"message_start", "message_delta", "usage", "message_stop"} {
		t.Run(eventType, func(t *testing.T) {
			for _, field := range fields {
				t.Run(field.name, func(t *testing.T) {
					for _, state := range []struct {
						name, usage string
						present     bool
					}{
						{"zero", fmt.Sprintf("{%q:0}", field.name), true},
						{"missing", "{}", false},
						{"null", fmt.Sprintf("{%q:null}", field.name), false},
					} {
						t.Run(state.name, func(t *testing.T) {
							want := usagePresenceSnapshotForTest{}
							if state.present {
								want = field.want
							}
							event := recvAnthropicUsageFrameForTest(t, eventType, state.usage)
							assertAnthropicUsagePresence(t, event, want)
						})
					}
				})
			}
		})
	}
}

func TestAnthropicStreamUsagePresenceCacheBucketsDoNotRequireInput(t *testing.T) {
	for _, eventType := range []string{"message_start", "message_delta", "usage", "message_stop"} {
		t.Run(eventType, func(t *testing.T) {
			for _, test := range []struct {
				name, usage string
				want        usagePresenceSnapshotForTest
			}{
				{
					name:  "cache_write_without_input",
					usage: `{"cache_creation_input_tokens":512}`,
					want:  usagePresenceSnapshotForTest{cacheCreation: 512, hasCacheCreation: true},
				},
				{
					name:  "cache_read_without_input",
					usage: `{"cache_read_input_tokens":1024}`,
					want:  usagePresenceSnapshotForTest{cacheRead: 1024, hasCacheRead: true},
				},
				{
					name:  "both_cache_buckets_with_null_input",
					usage: `{"input_tokens":null,"cache_creation_input_tokens":512,"cache_read_input_tokens":1024}`,
					want: usagePresenceSnapshotForTest{
						cacheCreation: 512, cacheRead: 1024,
						hasCacheCreation: true, hasCacheRead: true,
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					event := recvAnthropicUsageFrameForTest(t, eventType, test.usage)
					assertAnthropicUsagePresence(t, event, test.want)
				})
			}
		})
	}
}

func TestAnthropicStreamUsagePresenceMessageStopCarriesFinalSnapshotWithEOF(t *testing.T) {
	event := recvAnthropicUsageFrameForTest(t, "message_stop",
		`{"input_tokens":179,"output_tokens":74,"cache_creation_input_tokens":512,"cache_read_input_tokens":0}`)
	assertAnthropicUsagePresence(t, event, usagePresenceSnapshotForTest{
		input: 179, output: 74, cacheCreation: 512,
		hasInput: true, hasOutput: true, hasCacheCreation: true, hasCacheRead: true,
	})
}

func TestAnthropicStreamUsagePresenceMetadataFreeTerminators(t *testing.T) {
	for _, test := range []struct {
		name, body string
	}{
		{"message_stop", "data: {\"type\":\"message_stop\"}\n\n"},
		{"null_usage", "data: {\"type\":\"message_stop\",\"usage\":null}\n\n"},
		{"empty_usage", "data: {\"type\":\"message_stop\",\"usage\":{}}\n\n"},
		{"done_sentinel", "data: [DONE]\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := newAnthropicStream(io.NopCloser(strings.NewReader(test.body)))
			t.Cleanup(func() { _ = stream.Close() })
			event, err := stream.Recv()
			if !errors.Is(err, io.EOF) || event.Type != "message_stop" {
				t.Fatalf("Recv() = (%+v, %v), want message_stop with io.EOF", event, err)
			}
			assertAnthropicUsagePresence(t, event, usagePresenceSnapshotForTest{})
		})
	}
}

func TestAnthropicStreamUsagePresenceRejectsNegativeBuckets(t *testing.T) {
	for _, eventType := range []string{"message_start", "message_delta", "usage", "message_stop"} {
		t.Run(eventType, func(t *testing.T) {
			for _, field := range []string{
				"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
			} {
				t.Run(field, func(t *testing.T) {
					usage := fmt.Sprintf("{%q:-1}", field)
					stream := newAnthropicStream(io.NopCloser(strings.NewReader(anthropicUsageFrameForTest(eventType, usage))))
					t.Cleanup(func() { _ = stream.Close() })
					event, err := stream.Recv()
					if event.Type != "error" || (err == nil && event.Err == nil) {
						t.Fatalf("Recv() = (%+v, %v), want rejection of negative %s", event, err, field)
					}
					if !strings.Contains(fmt.Sprintf("%v %v", err, event.Err), "negative") {
						t.Errorf("Recv() errors = (%v, %v), want negative usage diagnostic", err, event.Err)
					}
					assertAnthropicUsagePresence(t, event, usagePresenceSnapshotForTest{})
				})
			}
		})
	}
}
