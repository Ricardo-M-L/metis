package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceAdapterFlushesTerminalTurnEventsToDisk(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal agent.Event
		wantKind string
	}{
		{name: "turn_end", terminal: agent.Event{Kind: agent.EventTurnEnd}, wantKind: `"kind":"turn_end"`},
		{name: "loop_done", terminal: agent.Event{Kind: agent.EventLoopDone, StopReason: "complete"}, wantKind: `"kind":"loop_done"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := session.NewTraceStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })

			adapter := NewTraceAdapter(store)
			adapter.SetSession("short-session")
			adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "short answer"})
			adapter.OnEvent(test.terminal)

			raw, err := os.ReadFile(filepath.Join(dir, "short-session.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"kind":"text"`) || !strings.Contains(string(raw), test.wantKind) {
				t.Fatalf("terminal event was not durably flushed: %q", raw)
			}
		})
	}
}

func TestTraceAdapterFlushPersistsInFlightBurst(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("interrupted-session")
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "partial response"})
	if err := adapter.Flush(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "interrupted-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind":"text"`) || !strings.Contains(string(raw), "partial response") {
		t.Fatalf("in-flight burst was not durably flushed: %q", raw)
	}
}

func TestChildLoopDoneDoesNotCloseTopLevelTurn(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("multi-agent-turn")
	adapter.OnEvent(agent.Event{
		Kind:      agent.EventToolStart,
		ToolName:  "Agent",
		ToolUseID: "spawn-1",
		ToolInput: map[string]any{"task": "inspect"},
	})
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventTurnEnd,
		SubAgentParentID: "spawn-1",
	})
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventLoopDone,
		StopReason:       "child_complete",
		SubAgentParentID: "spawn-1",
	})
	adapter.OnEvent(agent.Event{
		Kind:      agent.EventToolResult,
		ToolName:  "Agent",
		ToolUseID: "spawn-1",
		ToolResult: &agent.ToolResult{
			Output: "child result",
		},
	})

	events := store.Events("multi-agent-turn")
	if len(events) != 4 {
		t.Fatalf("events = %+v, want tool_start, child turn_end, child loop_done, tool_result", events)
	}
	for i, event := range events {
		if event.Turn != 1 {
			t.Fatalf("event[%d] kind=%q turn=%d, want parent turn 1", i, event.Kind, event.Turn)
		}
	}
	if got := store.CurrentTurn("multi-agent-turn"); got != 1 {
		t.Fatalf("current turn = %d, child LoopDone must not advance the top-level turn", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "multi-agent-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "child_complete") {
		t.Fatalf("child terminal event was not durably flushed: %q", raw)
	}

	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "parent_complete"})
	raw, err = os.ReadFile(filepath.Join(dir, "multi-agent-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind":"loop_done"`) {
		t.Fatalf("top-level LoopDone did not flush the completed turn: %q", raw)
	}
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Read", ToolUseID: "next"})
	events = store.Events("multi-agent-turn")
	if got := events[len(events)-1].Turn; got != 2 {
		t.Fatalf("event after top-level LoopDone has turn=%d, want next top-level turn 2", got)
	}
}

func TestTopLevelErrorClosesTurnBeforeNextUserMessage(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })
	adapter.SetSession("error-terminal")
	RecordUserMessage("error-terminal", "first")
	adapter.OnEvent(agent.Event{Kind: agent.EventError, Err: os.ErrDeadlineExceeded})
	RecordUserMessage("error-terminal", "second")

	events := store.Events("error-terminal")
	if len(events) != 3 {
		t.Fatalf("events = %+v, want first user, terminal error, second user", events)
	}
	if events[0].Turn != 1 || events[1].Turn != 1 || events[2].Turn != 2 {
		t.Fatalf("turns = [%d %d %d], want [1 1 2]", events[0].Turn, events[1].Turn, events[2].Turn)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "error-terminal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), os.ErrDeadlineExceeded.Error()) {
		t.Fatalf("terminal error was not durably flushed: %q", raw)
	}
}

func TestBoundTraceTurnKeepsPostErrorCleanupOnTerminalTurn(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })
	adapter.SetSession("post-error")
	RecordUserMessage("post-error", "first")
	ctx, origin := BindTraceTurn(context.Background(), "post-error")
	adapter.OnEvent(agent.Event{Kind: agent.EventError, Err: os.ErrDeadlineExceeded, TraceInvocationID: origin.InvocationID})
	adapter.OnEvent(agent.Event{Kind: agent.EventInfo, Info: "please resend", TraceInvocationID: origin.InvocationID})
	EndTraceTurn(ctx)
	RecordUserMessage("post-error", "second")

	events := store.Events("post-error")
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Turn != 1 || events[1].Turn != 1 || events[2].Turn != 1 || events[3].Turn != 2 {
		t.Fatalf("post-error turns = %+v", events)
	}
	if events[2].Text != "please resend" {
		t.Fatalf("cleanup event = %+v", events[2])
	}
}

func TestEndTraceTurnClosesRootWithoutTerminalEvent(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })
	adapter.SetSession("root-lifecycle")
	RecordUserMessage("root-lifecycle", "first")
	ctx, origin := BindTraceTurn(context.Background(), "root-lifecycle")
	adapter.OnEvent(agent.Event{Kind: agent.EventInfo, Info: "cleanup", TraceInvocationID: origin.InvocationID})
	EndTraceTurn(ctx)
	RecordUserMessage("root-lifecycle", "second")

	events := store.Events("root-lifecycle")
	if len(events) != 3 || events[0].Turn != 1 || events[1].Turn != 1 || events[2].Turn != 2 {
		t.Fatalf("root lifecycle did not close active turn: %+v", events)
	}
}

func TestChildErrorFlushesOriginWithoutClosingParentTurn(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("child-error")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "failing-child"})
	adapter.OnEvent(agent.Event{Kind: agent.EventError, Err: os.ErrPermission, SubAgentParentID: "failing-child"})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolResult, ToolName: "Agent", ToolUseID: "failing-child"})

	events := store.Events("child-error")
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	for i, event := range events {
		if event.Turn != 1 {
			t.Fatalf("event[%d] turn=%d, child error must not close parent turn", i, event.Turn)
		}
	}
	if events[1].SubAgentOf != "failing-child" {
		t.Fatalf("child error attribution = %+v", events[1])
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child-error.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "permission denied") {
		t.Fatalf("child error was not durably flushed: %q", raw)
	}
}

func TestLateBackgroundChildEventsStayOnOriginTurn(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("background-origin")
	adapter.OnEvent(agent.Event{
		Kind:      agent.EventToolStart,
		ToolName:  "Agent",
		ToolUseID: "background-agent",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "parent_complete"})

	// A background child may keep running after its parent turn has already
	// completed. Its terminal usage and LoopDone still belong to the spawn turn.
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventTokens,
		SubAgentParentID: "background-agent",
		InputTokens:      13,
		OutputTokens:     5,
	})
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventLoopDone,
		SubAgentParentID: "background-agent",
		StopReason:       "child_complete",
	})

	// The next top-level event must open turn 2, not turn 3 after a ghost child
	// turn consumed the next index.
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Read", ToolUseID: "next-user-turn"})

	events := store.Events("background-origin")
	if len(events) != 5 {
		t.Fatalf("events = %+v, want spawn, parent done, child tokens/done, next turn", events)
	}
	for _, index := range []int{0, 1, 2, 3} {
		if events[index].Turn != 1 {
			t.Fatalf("event[%d] kind=%q turn=%d, want origin turn 1", index, events[index].Kind, events[index].Turn)
		}
	}
	if events[2].SubAgentOf != "background-agent" || events[3].SubAgentOf != "background-agent" {
		t.Fatalf("late child attribution = tokens:%q done:%q", events[2].SubAgentOf, events[3].SubAgentOf)
	}
	if got := events[4].Turn; got != 2 {
		t.Fatalf("next top-level turn = %d, want 2", got)
	}
	if got := store.CurrentTurn("background-origin"); got != 2 {
		t.Fatalf("current turn = %d, want 2", got)
	}
}

func TestBackgroundChildEventsStayInOriginSessionAfterSwitch(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("session-a")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "agent-a"})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "parent_complete"})

	adapter.SetSession("session-b")
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "session b answer"})
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventTextDelta,
		TextDelta:        "late child answer",
		SubAgentParentID: "agent-a",
	})
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventTokens,
		SubAgentParentID: "agent-a",
		InputTokens:      21,
		OutputTokens:     8,
	})
	adapter.OnEvent(agent.Event{
		Kind:             agent.EventLoopDone,
		SubAgentParentID: "agent-a",
		StopReason:       "child_complete",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "session_b_complete"})

	aEvents := store.Events("session-a")
	bEvents := store.Events("session-b")
	if len(aEvents) != 5 {
		t.Fatalf("session A events = %+v, want spawn, parent done, child text/tokens/done", aEvents)
	}
	for _, index := range []int{2, 3, 4} {
		if aEvents[index].Turn != 1 || aEvents[index].SubAgentOf != "agent-a" {
			t.Fatalf("session A child event[%d] = %+v, want origin session/turn/parent", index, aEvents[index])
		}
	}
	if got := aEvents[2].Text; got != "late child answer" {
		t.Fatalf("child text = %q", got)
	}
	if len(bEvents) != 2 || bEvents[0].Text != "session b answer" || bEvents[1].Kind != "loop_done" {
		t.Fatalf("session B was polluted by late child events: %+v", bEvents)
	}

	// The late child terminal event must be flushed to its origin file even
	// though session B is now active.
	raw, err := os.ReadFile(filepath.Join(dir, "session-a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "child_complete") || !strings.Contains(string(raw), "late child answer") {
		t.Fatalf("origin session did not durably flush late child completion: %q", raw)
	}
}

func TestParentAndChildTextBurstsDoNotMerge(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("burst-owner")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "child-owner"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "parent text"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "child text", SubAgentParentID: "child-owner"})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "parent_complete"})

	events := store.Events("burst-owner")
	if len(events) != 4 {
		t.Fatalf("events = %+v, want spawn, distinct parent/child text, done", events)
	}
	if events[1].Kind != "text" || events[1].Text != "parent text" || events[1].SubAgentOf != "" {
		t.Fatalf("parent burst = %+v", events[1])
	}
	if events[2].Kind != "text" || events[2].Text != "child text" || events[2].SubAgentOf != "child-owner" {
		t.Fatalf("child burst = %+v", events[2])
	}
}

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

func TestTraceAdapterPersistsTraceCallID(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewTraceAdapter(store)
	adapter.SetSession("call-pair")
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Bash", ToolUseID: "provider-reused", TraceCallID: "call-1",
	})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolResult, ToolName: "Bash", ToolUseID: "provider-reused", TraceCallID: "call-1",
		ToolResult: &agent.ToolResult{Output: "ok"},
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	events := reopened.Events("call-pair")
	if len(events) != 2 || events[0].TraceCallID != "call-1" || events[1].TraceCallID != "call-1" {
		t.Fatalf("trace call ID did not survive persistence: %+v", events)
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

func TestTraceInvocationIDsDisambiguateDuplicateProviderIDsAcrossSessions(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("session-a")
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "duplicate",
		TraceInvocationID: "invocation-a",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "parent-a"})

	adapter.SetSession("session-b")
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "duplicate",
		TraceInvocationID: "invocation-b",
	})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTextDelta, TextDelta: "late a",
		SubAgentParentID: "duplicate", TraceInvocationID: "invocation-a",
	})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTextDelta, TextDelta: "live b",
		SubAgentParentID: "duplicate", TraceInvocationID: "invocation-b",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "parent-b"})

	aEvents := store.Events("session-a")
	bEvents := store.Events("session-b")
	if len(aEvents) != 3 || aEvents[2].Text != "late a" || aEvents[2].Turn != 1 {
		t.Fatalf("session A lost its duplicate-id child: %+v", aEvents)
	}
	if len(bEvents) != 3 || bEvents[1].Text != "live b" || bEvents[1].Turn != 1 {
		t.Fatalf("session B duplicate-id child = %+v", bEvents)
	}
}

func TestTraceInvocationIDsDisambiguateDuplicateProviderIDsAcrossTurns(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("same-session")
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "duplicate",
		TraceInvocationID: "turn-one-invocation",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "turn one parent done"})

	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "duplicate",
		TraceInvocationID: "turn-two-invocation",
	})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTextDelta, TextDelta: "late turn one child",
		SubAgentParentID: "duplicate", TraceInvocationID: "turn-one-invocation",
	})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTextDelta, TextDelta: "live turn two child",
		SubAgentParentID: "duplicate", TraceInvocationID: "turn-two-invocation",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "turn two parent done"})

	events := store.Events("same-session")
	var lateTurn, liveTurn int
	for _, event := range events {
		switch event.Text {
		case "late turn one child":
			lateTurn = event.Turn
		case "live turn two child":
			liveTurn = event.Turn
		}
	}
	if lateTurn != 1 || liveTurn != 2 {
		t.Fatalf("duplicate provider ID turn ownership = late:%d live:%d events:%+v", lateTurn, liveTurn, events)
	}
}

func TestTraceInvocationIDsSeparateParallelChildBurstsWithDuplicateProviderIDs(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("parallel")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "duplicate", TraceInvocationID: "parallel-1"})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "duplicate", TraceInvocationID: "parallel-2"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "one", SubAgentParentID: "duplicate", TraceInvocationID: "parallel-1"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "two", SubAgentParentID: "duplicate", TraceInvocationID: "parallel-2"})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "done"})

	events := store.Events("parallel")
	if len(events) != 5 {
		t.Fatalf("parallel events = %+v", events)
	}
	if events[2].Text != "one" || events[3].Text != "two" {
		t.Fatalf("parallel child bursts merged or reordered: %+v", events)
	}
	nodes := store.Trace("parallel")
	if len(nodes) != 5 || nodes[1].Event.Text != "one" || nodes[1].Depth != 1 ||
		nodes[3].Event.Text != "two" || nodes[3].Depth != 1 {
		t.Fatalf("persisted invocation tree lost or merged child bursts: %+v", nodes)
	}
}

func TestForkAndRepeatedRalphChildrenKeepTraceOrigin(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("fork-ralph")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Fork", ToolUseID: "fork-public", TraceInvocationID: "fork-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "fork child", SubAgentParentID: "fork-public", TraceInvocationID: "fork-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolResult, ToolName: "Fork", ToolUseID: "fork-public", TraceInvocationID: "fork-internal"})

	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Ralph", ToolUseID: "ralph-public", TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationStart, TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "round one", SubAgentParentID: "ralph-public", TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationEnd, TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationStart, TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "round two", SubAgentParentID: "ralph-public", TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationEnd, TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolResult, ToolName: "Ralph", ToolUseID: "ralph-public", TraceInvocationID: "ralph-internal"})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "done"})

	events := store.Events("fork-ralph")
	var forkText, firstRound, secondRound bool
	for _, ev := range events {
		switch ev.Text {
		case "fork child":
			forkText = ev.SubAgentOf == "fork-public"
		case "round one":
			firstRound = ev.SubAgentOf == "ralph-public"
		case "round two":
			secondRound = ev.SubAgentOf == "ralph-public"
		}
	}
	if !forkText || !firstRound || !secondRound {
		t.Fatalf("Fork/Ralph child origins missing: %+v", events)
	}
}

func TestNestedRalphAgentLifecycleRetainsOriginUntilBothRunnersExit(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("nested-ralph")
	const invocationID = "nested-ralph-internal"
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Ralph", ToolUseID: "ralph-public",
		TraceInvocationID: invocationID,
	})
	// Ralph and the Agent it delegates to intentionally share the dispatch
	// invocation ID, so two independent runners are now alive.
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationStart, TraceInvocationID: invocationID})
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationStart, TraceInvocationID: invocationID})
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationEnd, TraceInvocationID: invocationID})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolResult, ToolName: "Ralph", ToolUseID: "ralph-public",
		TraceInvocationID: invocationID, ToolResult: &agent.ToolResult{Output: "outer result"},
	})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventInfo, Info: "inner runner cleanup", SubAgentParentID: "ralph-public",
		TraceInvocationID: invocationID,
	})
	if _, ok := adapter.invocationOrigin[invocationID]; !ok {
		t.Fatal("nested invocation origin was removed while inner Agent was still running")
	}

	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationEnd, TraceInvocationID: invocationID})
	if _, ok := adapter.invocationOrigin[invocationID]; ok {
		t.Fatal("nested invocation origin leaked after result and both lifecycle ends")
	}
	adapter.OnEvent(agent.Event{
		Kind: agent.EventInfo, Info: "stale cleanup must drop", SubAgentParentID: "ralph-public",
		TraceInvocationID: invocationID,
	})

	events := store.Events("nested-ralph")
	var retained, stale bool
	for _, ev := range events {
		switch ev.Text {
		case "inner runner cleanup":
			retained = true
		case "stale cleanup must drop":
			stale = true
		}
	}
	if !retained || stale {
		t.Fatalf("nested lifecycle retention = retained:%v stale:%v events:%+v", retained, stale, events)
	}
}

func TestUnstartedTraceInvocationIsCleanedUpForDeniedOrShortCircuitedCall(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("unstarted")
	for _, tc := range []struct {
		name     string
		internal string
		public   string
		output   string
	}{
		{name: "permission denied", internal: "denied-internal", public: "denied-public", output: "denied"},
		{name: "hook short circuit", internal: "hook-internal", public: "hook-public", output: "hook supplied result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: tc.public, TraceInvocationID: tc.internal})
			adapter.OnEvent(agent.Event{
				Kind: agent.EventToolResult, ToolName: "Agent", ToolUseID: tc.public, TraceInvocationID: tc.internal,
				ToolResult: &agent.ToolResult{Output: tc.output, IsError: tc.name == "permission denied"},
			})
			if _, ok := adapter.invocationOrigin[tc.internal]; ok {
				t.Fatalf("unstarted %s invocation leaked an origin entry", tc.name)
			}
		})
	}
}

func TestNestedBackgroundGrandchildStaysAtOriginAfterSessionSwitch(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adapter := NewTraceAdapter(store)
	adapter.SetSession("origin")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "parent-public", TraceInvocationID: "parent-internal"})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "grandchild-public",
		SubAgentParentID: "parent-public", TraceInvocationID: "grandchild-internal", TraceParentInvocationID: "parent-internal",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "root-finished"})
	adapter.SetSession("other")
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTextDelta, TextDelta: "late grandchild",
		SubAgentParentID: "grandchild-public", TraceInvocationID: "grandchild-internal",
	})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "other-finished"})

	originEvents := store.Events("origin")
	otherEvents := store.Events("other")
	if len(originEvents) != 4 || originEvents[3].Text != "late grandchild" || originEvents[3].SubAgentOf != "grandchild-public" {
		t.Fatalf("nested late grandchild attribution = %+v", originEvents)
	}
	if len(otherEvents) != 1 || otherEvents[0].Kind != "loop_done" {
		t.Fatalf("other session polluted by grandchild: %+v", otherEvents)
	}
}

func TestBindTraceTurnAndResolvedObserverPinLateTokenUsage(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })
	adapter.SetSession("origin")
	RecordUserMessage("origin", "query")
	ctx, bound := BindTraceTurn(context.Background(), "origin")
	traceID := agent.TraceInvocationIDFromContext(ctx)
	if traceID == "" || bound.SessionID != "origin" || bound.Turn != 1 || bound.InvocationID != traceID {
		t.Fatalf("BindTraceTurn = ctx id %q, origin %+v", traceID, bound)
	}

	var observed []ResolvedTraceEvent
	adapter.SetResolvedEventObserver(func(ev ResolvedTraceEvent) {
		observed = append(observed, ev)
	})
	adapter.SetSession("other")
	adapter.OnEvent(agent.Event{Kind: agent.EventTokens, TraceInvocationID: traceID, InputTokens: 10, OutputTokens: 2})
	adapter.OnEvent(agent.Event{Kind: agent.EventTokens, TraceInvocationID: traceID, InputTokens: 4, OutputTokens: 3, CacheReadInputTokens: 7})

	originEvents := store.Events("origin")
	if len(originEvents) != 3 || originEvents[1].Kind != "tokens" || originEvents[2].Kind != "tokens" {
		t.Fatalf("late tokens were not pinned to origin: %+v", originEvents)
	}
	if len(store.Events("other")) != 0 {
		t.Fatalf("late tokens polluted active session: %+v", store.Events("other"))
	}
	if len(observed) != 2 {
		t.Fatalf("observer events = %+v", observed)
	}
	last := observed[1]
	if last.SessionID != "origin" || last.Turn != 1 || last.CumulativeUsage.InputTokens != 14 || last.CumulativeUsage.OutputTokens != 5 || last.CumulativeUsage.CacheReadTokens != 7 {
		t.Fatalf("resolved cumulative usage = %+v", last)
	}
}

func TestBindTraceTurnKeepsConcurrentSessionTurnsIndependent(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })

	adapter.SetSession("session-a")
	RecordUserMessage("session-a", "a")
	ctxA, originA := BindTraceTurn(context.Background(), "session-a")
	if originA.Turn != 1 {
		t.Fatalf("session A turn = %d, want 1", originA.Turn)
	}

	adapter.SetSession("session-b")
	RecordUserMessage("session-b", "b")
	_, originB := BindTraceTurn(context.Background(), "session-b")
	if originB.Turn != 1 {
		t.Fatalf("session B turn = %d, want 1", originB.Turn)
	}

	// Session A completes after the Desktop has switched to B. Its terminal
	// event must close only A's active turn, not the independently running B
	// turn. Rebinding B while it is still live must therefore return turn 1.
	adapter.OnEvent(agent.Event{
		Kind:              agent.EventLoopDone,
		StopReason:        "a complete",
		TraceInvocationID: agent.TraceInvocationIDFromContext(ctxA),
	})
	_, reboundB := BindTraceTurn(context.Background(), "session-b")
	if reboundB.Turn != originB.Turn {
		t.Fatalf("late A completion changed B turn from %d to %d", originB.Turn, reboundB.Turn)
	}
}

func TestRecordAndBindExplicitSessionNeverReuseAnotherSessionTurn(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })

	adapter.SetSession("selected")
	RecordUserMessage("selected", "selected query")
	// Simulate two durable turns already present for an explicit background
	// session. RecordUserMessage and BindTraceTurn receive that session ID and
	// must advance it to 3 rather than borrowing selected's active turn 1.
	store.NextTurn("background")
	store.NextTurn("background")
	RecordUserMessage("background", "background query")
	_, origin := BindTraceTurn(context.Background(), "background")
	if origin.Turn != 3 {
		t.Fatalf("background origin turn = %d, want 3", origin.Turn)
	}
	events := store.Events("background")
	if len(events) != 1 || events[0].Turn != 3 {
		t.Fatalf("background user anchor = %+v, want turn 3", events)
	}
}
