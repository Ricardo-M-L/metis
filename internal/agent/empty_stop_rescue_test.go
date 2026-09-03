package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type incompleteTerminalMemorySpy struct {
	memory.Repository
	records atomic.Int32
}

func (*incompleteTerminalMemorySpy) BuildContext() string { return "" }

func (s *incompleteTerminalMemorySpy) RecordTurn(context.Context, string, string, string, string) error {
	s.records.Add(1)
	return nil
}

func emptyStopStream(stopReason string) llm.StreamReader {
	return &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "message_delta", StopReason: stopReason},
		{Type: "message_stop"},
	}}
}

func TestHasUserFacingText_TextBlock(t *testing.T) {
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "text", Text: "found it: line 42 has the bug"},
	})
	if !yes {
		t.Error("non-empty text block should count as user-facing")
	}
}

func TestHasUserFacingText_EmptyContent(t *testing.T) {
	if hasUserFacingText(nil) {
		t.Error("nil content has no text")
	}
	if hasUserFacingText([]llm.ContentBlock{}) {
		t.Error("empty content slice has no text")
	}
}

func TestHasUserFacingText_WhitespaceOnlyIsEmpty(t *testing.T) {
	cases := []string{"", " ", "\n", "\t  \n  "}
	for _, w := range cases {
		t.Run("ws="+strings.ReplaceAll(w, "\n", "\\n"), func(t *testing.T) {
			got := hasUserFacingText([]llm.ContentBlock{{Type: "text", Text: w}})
			if got {
				t.Errorf("whitespace-only text %q should not count", w)
			}
		})
	}
}

func TestHasUserFacingText_ToolUseAlone(t *testing.T) {
	// The Bug B case: model emitted only tool_use blocks, no text.
	// Rescue must fire.
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "tool_use", ToolName: "Read", ToolInput: map[string]any{"path": "/foo"}},
		{Type: "tool_use", ToolName: "Grep", ToolInput: map[string]any{"pattern": "bar"}},
	})
	if yes {
		t.Error("tool_use-only content has no user-facing text — rescue should fire")
	}
}

func TestHasUserFacingText_ThinkingDoesNotCount(t *testing.T) {
	// `thinking` blocks are internal — the user never sees them. A
	// stop with only thinking + tool_use is still a silent stop.
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "thinking", Text: "let me check the file"},
		{Type: "tool_use", ToolName: "Read", ToolInput: map[string]any{"path": "/x"}},
	})
	if yes {
		t.Error("thinking blocks should not satisfy the user-facing-text check")
	}
}

func TestHasUserFacingText_MixedBlocks(t *testing.T) {
	// Real assistant turn: thinking + tool_use + final text. The
	// text block at the end is what the user actually reads.
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "thinking", Text: "internal"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "text", Text: "done — see line 42"},
	})
	if !yes {
		t.Error("mixed content with a final text block should count as user-facing")
	}
}

func TestEmptyStopRescueMessage_ContainsKeyDirectives(t *testing.T) {
	// Pin the directive shape: must tell model to summarize and not
	// restart investigation. If a future refactor accidentally drops
	// "don't restart" the model will happily redo the whole thing.
	msg := emptyStopRescueMessage
	for _, want := range []string{
		"1-3 lines",
		"Don't restart",
		"system-reminder",
		"blank screen",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rescue message missing %q; got:\n%s", want, msg)
		}
	}
}

func TestLoopRun_EmptyFinalRescueOmitsToolsAndBlankAssistant(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("end_turn"),
		textStream("summary after rescue"),
	}}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("do the task")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(requests))
	}
	if len(requests[0].Tools) == 0 {
		t.Fatal("initial request unexpectedly omitted tools")
	}
	if len(requests[1].Tools) != 0 {
		t.Fatalf("rescue request exposed %d tools, want 0", len(requests[1].Tools))
	}
	if !requestContains(requests[1], emptyStopRescueMessage) {
		t.Fatal("rescue request omitted the summary instruction")
	}
	for _, message := range requests[1].Messages {
		if message.Role == llm.RoleAssistant && !hasUserFacingText(message.Content) && !containsToolUseBlock(message.Content) {
			t.Fatal("rescue request replayed an empty assistant message")
		}
	}

	var stop string
	for event := range out {
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", stop)
	}
}

func TestLoopRun_EmptyFinalRescuePreservesProviderContinuationState(t *testing.T) {
	tests := []struct {
		name      string
		stateType string
		first     llm.StreamEvent
		check     func(llm.ContentBlock) bool
	}{
		{
			name:      "provider state",
			stateType: "provider_state",
			first: llm.StreamEvent{Type: "provider_state", ProviderHint: map[string]string{
				"openai.responses.response_id": "resp-42",
			}},
			check: func(block llm.ContentBlock) bool {
				return block.ProviderHint["openai.responses.response_id"] == "resp-42"
			},
		},
		{
			name:      "redacted thinking",
			stateType: "redacted_thinking",
			first: llm.StreamEvent{Type: "redacted_thinking", TextDelta: "opaque-ciphertext", ProviderHint: map[string]string{
				"openai.responses.item_id": "reasoning-42",
			}},
			check: func(block llm.ContentBlock) bool {
				return block.Data == "opaque-ciphertext" && block.ProviderHint["openai.responses.item_id"] == "reasoning-42"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &queuedStreamProvider{streams: []llm.StreamReader{
				&loopRegressionStream{events: []llm.StreamEvent{
					test.first,
					{Type: "message_delta", StopReason: "end_turn"},
					{Type: "message_stop"},
				}},
				textStream("summary after stateful rescue"),
			}}
			loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
			loop.AppendUser("do the task")

			out := make(chan Event, 64)
			if err := loop.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			close(out)

			requests := provider.capturedRequests()
			if len(requests) != 2 {
				t.Fatalf("provider calls = %d, want 2", len(requests))
			}
			var preserved bool
			for _, message := range requests[1].Messages {
				if message.Role != llm.RoleAssistant {
					continue
				}
				for _, block := range message.Content {
					if block.Type == test.stateType && test.check(block) {
						preserved = true
					}
				}
			}
			if !preserved {
				t.Fatalf("rescue request dropped %s continuation state", test.stateType)
			}
		})
	}
}

func TestLoopRun_SecondEmptyFinalIsVisibleIncompleteResult(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("end_turn"),
		emptyStopStream("end_turn"),
	}}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("do the task")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var text, stop string
	var sawTerminalInfo bool
	for event := range out {
		if event.Kind == EventTextDelta {
			text += event.TextDelta
		}
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
		if event.Kind == EventInfo && strings.Contains(event.Info, "empty-final-answer failure") {
			sawTerminalInfo = true
		}
	}
	if !strings.Contains(text, emptyStopFallbackMessage) {
		t.Fatalf("visible output %q omitted local fallback", text)
	}
	if stop != "empty_final_answer" {
		t.Fatalf("stop reason = %q, want empty_final_answer", stop)
	}
	if !sawTerminalInfo {
		t.Fatal("terminal empty-final failure did not emit a diagnostic")
	}
	for _, message := range loop.History() {
		if message.Role == llm.RoleAssistant && !hasUserFacingText(message.Content) && !containsToolUseBlock(message.Content) {
			t.Fatal("history retained an empty assistant message")
		}
	}
}

func TestLoopRun_NewUserTurnRecoversAfterEmptyFinalFailure(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("end_turn"),
		emptyStopStream("end_turn"),
		textStream("recovered on the next user turn"),
	}}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("first request")
	firstEvents := make(chan Event, 64)
	if err := loop.Run(context.Background(), firstEvents); err != nil {
		t.Fatal(err)
	}
	close(firstEvents)

	loop.AppendUser("retry now")
	secondEvents := make(chan Event, 64)
	if err := loop.Run(context.Background(), secondEvents); err != nil {
		t.Fatal(err)
	}
	close(secondEvents)
	var text, stop string
	for event := range secondEvents {
		if event.Kind == EventTextDelta {
			text += event.TextDelta
		}
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if text != "recovered on the next user turn" || stop != "end_turn" {
		t.Fatalf("second turn text=%q stop=%q", text, stop)
	}
	requests := provider.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(requests))
	}
	if len(requests[2].Tools) == 0 {
		t.Fatal("ordinary tools did not return after the one-shot rescue request")
	}
}

func TestLoopRun_ToolUseStopWithoutToolBlockIsProtocolError(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("tool_use"),
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("do the task")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var sawMismatch bool
	var stop string
	for event := range out {
		if event.Kind == EventInfo && strings.Contains(event.Info, "without a tool_use block") {
			sawMismatch = true
		}
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if !sawMismatch {
		t.Fatal("provider protocol mismatch was not reported")
	}
	if stop != "provider_protocol_error" {
		t.Fatalf("stop reason = %q, want provider_protocol_error", stop)
	}
	if len(provider.capturedRequests()) != 1 {
		t.Fatalf("provider calls = %d, want no retry after protocol error", len(provider.capturedRequests()))
	}
}

func TestLoopRun_ToollessRescueRefusesProviderToolCall(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("end_turn"),
		&loopRegressionStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "unexpected", ToolName: "LowOutput"},
			{Type: "tool_input_delta", ToolUseID: "unexpected", InputDelta: `{"payload":"must-not-run"}`},
			{Type: "tool_use_stop", ToolUseID: "unexpected", InputDelta: `{"payload":"must-not-run"}`},
			{Type: "provider_state", ProviderHint: map[string]string{
				"openai.responses.response_id": "resp-with-unaccepted-call",
				"openai.responses.state_key":   "state-key",
			}},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}},
	}}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("do the task")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var sawRefusal, sawToolStart bool
	var stop string
	for event := range out {
		if event.Kind == EventInfo && strings.Contains(event.Info, "refusing the calls") {
			sawRefusal = true
		}
		if event.Kind == EventToolStart {
			sawToolStart = true
		}
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if !sawRefusal {
		t.Fatal("unexpected rescue tool call was not reported")
	}
	if sawToolStart {
		t.Fatal("tool emitted for a tool-less rescue request was dispatched")
	}
	if stop != "provider_protocol_error" {
		t.Fatalf("stop reason = %q, want provider_protocol_error", stop)
	}
	for _, message := range loop.History() {
		if containsToolUseBlock(message.Content) {
			t.Fatal("unexpected rescue tool call remained in history")
		}
		for _, block := range message.Content {
			if block.Type == "provider_state" {
				t.Fatal("provider continuation for an unaccepted call remained in history")
			}
		}
	}
}

func TestLoopRun_ToollessRescueRefusesMixedTextAndStructuredToolCall(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("end_turn"),
		&loopRegressionStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "I tried to call a tool."},
			{Type: "tool_use_start", ToolUseID: "unexpected-mixed", ToolName: "LowOutput"},
			{Type: "tool_input_delta", ToolUseID: "unexpected-mixed", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "unexpected-mixed", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}},
	}}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("do the task")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var stop string
	var sawToolStart bool
	for event := range out {
		if event.Kind == EventToolStart {
			sawToolStart = true
		}
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if sawToolStart {
		t.Fatal("structured tool call from tool-less rescue was dispatched")
	}
	if stop != "provider_protocol_error" {
		t.Fatalf("stop reason = %q, want provider_protocol_error", stop)
	}
	if len(provider.capturedRequests()) != 2 {
		t.Fatalf("provider calls = %d, want bounded two-call rescue", len(provider.capturedRequests()))
	}
}

func TestLoopRun_ToollessRescueRefusesPlainTextToolRecovery(t *testing.T) {
	var calls atomic.Int32
	registry := tools.NewRegistry()
	registry.Register(recoveredNativeTool{calls: &calls})
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		emptyStopStream("end_turn"),
		textStream(plainTextRecoveredCall),
	}}
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "sys", 10)
	loop.RecoverTextToolCalls = true
	loop.AppendUser("do the task")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var stop string
	for event := range out {
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("plain-text rescue tool call executed %d time(s)", calls.Load())
	}
	if stop != "provider_protocol_error" {
		t.Fatalf("stop reason = %q, want provider_protocol_error", stop)
	}
	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want bounded two-call rescue", len(requests))
	}
	if len(requests[1].Tools) != 0 {
		t.Fatalf("rescue request exposed %d tools", len(requests[1].Tools))
	}
}

func TestLoopRun_IncompleteProviderStopDoesNotRetryOrExecuteTools(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		events     []llm.StreamEvent
	}{
		{
			name:       "empty max tokens",
			stopReason: "max_tokens",
			events: []llm.StreamEvent{
				{Type: "message_delta", StopReason: "max_tokens"},
				{Type: "message_stop"},
			},
		},
		{
			name:       "filtered partial with tool",
			stopReason: "content_filter",
			events: []llm.StreamEvent{
				{Type: "text_delta", TextDelta: "partial"},
				{Type: "tool_use_start", ToolUseID: "filtered-call", ToolName: "LowOutput"},
				{Type: "tool_input_delta", ToolUseID: "filtered-call", InputDelta: `{}`},
				{Type: "tool_use_stop", ToolUseID: "filtered-call", InputDelta: `{}`},
				{Type: "provider_state", ProviderHint: map[string]string{"openai.responses.response_id": "filtered-response"}},
				{Type: "message_delta", StopReason: "content_filter"},
				{Type: "message_stop"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &queuedStreamProvider{streams: []llm.StreamReader{
				&loopRegressionStream{events: test.events},
			}}
			registry := tools.NewRegistry()
			registry.Register(lowOutputTool{})
			loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "sys", 10)
			memorySpy := &incompleteTerminalMemorySpy{}
			loop.Memory = memorySpy
			loop.AppendUser("do the task")

			out := make(chan Event, 64)
			if err := loop.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			close(out)
			var stop string
			var sawToolStart bool
			for event := range out {
				if event.Kind == EventToolStart {
					sawToolStart = true
				}
				if event.Kind == EventLoopDone {
					stop = event.StopReason
				}
			}
			if stop != test.stopReason {
				t.Fatalf("stop = %q, want %q", stop, test.stopReason)
			}
			if sawToolStart {
				t.Fatal("tool from incomplete provider response was executed")
			}
			if len(provider.capturedRequests()) != 1 {
				t.Fatalf("provider calls = %d, want no retry", len(provider.capturedRequests()))
			}
			if memorySpy.records.Load() != 0 {
				t.Fatalf("incomplete turn was persisted as completed memory %d time(s)", memorySpy.records.Load())
			}
			for _, message := range loop.History() {
				for _, block := range message.Content {
					if block.Type == "tool_use" || block.Type == "provider_state" {
						t.Fatalf("unaccepted incomplete call state remained in history: %+v", block)
					}
				}
			}
		})
	}
}

func TestLoopRun_CanceledContextCannotReturnSuccessAtTerminalRace(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{textStream("finished")}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "sys", 2)
	loop.AppendUser("answer")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := make(chan Event, 16)
	err := loop.Run(ctx, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}
