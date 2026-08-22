package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceAdapterSeparatesThinkingAndTextBurstsInOrder(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("thinking-order")
	adapter.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "inspect "})
	adapter.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "state"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "answer"})
	adapter.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "verify"})
	adapter.OnEvent(agent.Event{
		Kind:      agent.EventToolStart,
		ToolName:  "Bash",
		ToolUseID: "tool-1",
		ToolInput: map[string]any{"command": "echo ok"},
	})

	events := store.Events("thinking-order")
	wantKinds := []string{"thinking", "text", "thinking", "tool_start"}
	wantTexts := []string{"inspect state", "answer", "verify", `{"command":"echo ok"}`}
	if len(events) != len(wantKinds) {
		t.Fatalf("events = %+v, want kinds %v", events, wantKinds)
	}
	for i, event := range events {
		if event.Kind != wantKinds[i] || event.Text != wantTexts[i] {
			t.Fatalf("event[%d] = (%q, %q), want (%q, %q)", i, event.Kind, event.Text, wantKinds[i], wantTexts[i])
		}
		if event.Turn != 1 {
			t.Fatalf("event[%d].Turn = %d, want 1", i, event.Turn)
		}
	}
}

func TestTraceAdapterStoresRedactedThinkingAsSafeStandaloneEvent(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const cipherText = "EuwBCkAG-SECRET-CIPHERTEXT=="
	adapter := NewTraceAdapter(store)
	adapter.SetSession("redacted-thinking")
	adapter.OnEvent(agent.Event{Kind: agent.EventThinkingDelta, TextDelta: "visible provider summary"})
	adapter.OnEvent(agent.Event{Kind: agent.EventRedactedThinking, TextDelta: cipherText})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "safe answer"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTurnEnd})

	events := store.Events("redacted-thinking")
	wantKinds := []string{"thinking", "thinking_redacted", "text", "turn_end"}
	if len(events) != len(wantKinds) {
		t.Fatalf("events = %+v, want kinds %v", events, wantKinds)
	}
	for i, event := range events {
		if event.Kind != wantKinds[i] {
			t.Fatalf("event[%d].Kind = %q, want %q", i, event.Kind, wantKinds[i])
		}
		if strings.Contains(event.Text, cipherText) {
			t.Fatalf("event[%d] leaked provider ciphertext: %q", i, event.Text)
		}
	}
	if got, want := events[1].Text, "Reasoning redacted by provider"; got != want {
		t.Fatalf("redacted placeholder = %q, want %q", got, want)
	}
}
