package transcript

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func userText(s string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: s}}}
}

func assistantText(s string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: s}}}
}

func toolUseBlock(id, name string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: map[string]any{}},
	}}
}

func toolResultBlock(id, output string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: id, ToolResult: output},
	}}
}

func TestUndoWithPrefill_ReturnsLastUserText(t *testing.T) {
	msgs := []llm.Message{
		userText("first prompt"),
		assistantText("first reply"),
		userText("second prompt that I want to edit"),
		assistantText("second reply"),
	}
	out, prefill, ok := UndoWithPrefill(msgs)
	if !ok {
		t.Fatal("UndoWithPrefill should ok=true when there's a turn to undo")
	}
	if prefill != "second prompt that I want to edit" {
		t.Errorf("prefill = %q; want second prompt verbatim", prefill)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d; want 2 (only first turn left)", len(out))
	}
}

func TestUndoWithPrefill_NotokOnEmpty(t *testing.T) {
	out, prefill, ok := UndoWithPrefill(nil)
	if ok {
		t.Error("UndoWithPrefill on empty msgs should ok=false")
	}
	if prefill != "" {
		t.Errorf("prefill on no-op should be empty; got %q", prefill)
	}
	if out != nil {
		t.Errorf("out on no-op should be nil; got %+v", out)
	}
}

func TestUndoWithPrefill_LastBlockOfMultiBlockUserMsg(t *testing.T) {
	// A user message with two text blocks (e.g. a system context block
	// prepended ahead of the user's actual question). We return the
	// LAST text block — that's what the user typed.
	msgs := []llm.Message{
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: "text", Text: "[context: today is Tuesday]"},
				{Type: "text", Text: "what should I work on?"},
			},
		},
		assistantText("Reply A"),
	}
	_, prefill, ok := UndoWithPrefill(msgs)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if prefill != "what should I work on?" {
		t.Errorf("prefill = %q; want last text block only", prefill)
	}
}

func TestUndoWithPrefill_SkipsToolResultUserMessages(t *testing.T) {
	// Tool-result user messages must NOT be returned as prefill.
	msgs := []llm.Message{
		userText("real user prompt"),
		toolUseBlock("1", "Bash"),
		toolResultBlock("1", "tool output"),
		assistantText("done"),
	}
	out, prefill, ok := UndoWithPrefill(msgs)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if prefill != "real user prompt" {
		t.Errorf("prefill = %q; want plain-user prompt only", prefill)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v; want empty (everything popped)", out)
	}
}

func TestUndoWithPrefill_SkipsSyntheticReminderMessages(t *testing.T) {
	msgs := []llm.Message{
		userText("real user prompt"),
		assistantText(""),
		userText("<system-reminder>internal rescue</system-reminder>"),
		assistantText("final answer"),
	}
	out, prefill, ok := UndoWithPrefill(msgs)
	if !ok || prefill != "real user prompt" {
		t.Fatalf("UndoWithPrefill = (%q, %v), want real user prompt", prefill, ok)
	}
	if len(out) != 0 {
		t.Fatalf("synthetic reminder split the turn: %+v", out)
	}
}

func TestUndoWithPrefill_StripsPrependedReminderButKeepsUserText(t *testing.T) {
	msgs := []llm.Message{
		userText("<system-reminder>plan policy</system-reminder>\n\nimplement the feature"),
		assistantText("done"),
	}
	_, prefill, ok := UndoWithPrefill(msgs)
	if !ok || prefill != "implement the feature" {
		t.Fatalf("UndoWithPrefill = (%q, %v), want visible prompt", prefill, ok)
	}
}

func TestCountTurns_IgnoresSyntheticUserMessages(t *testing.T) {
	msgs := []llm.Message{
		userText("first"),
		assistantText("working"),
		userText("<job_notification>done</job_notification>"),
		userText("<system-reminder>internal</system-reminder>"),
		assistantText("finished"),
	}
	if got := CountTurns(msgs); got != 1 {
		t.Fatalf("CountTurns = %d, want 1 real user turn", got)
	}
}

func TestUndo_PopsTheLastTurn(t *testing.T) {
	msgs := []llm.Message{
		userText("first prompt"),
		assistantText("first reply"),
		userText("second prompt"),
		assistantText("second reply"),
	}
	out, ok := Undo(msgs)
	if !ok {
		t.Fatal("Undo should report ok=true when a turn exists")
	}
	if len(out) != 2 {
		t.Fatalf("Undo result len = %d, want 2", len(out))
	}
	if out[0].Content[0].Text != "first prompt" || out[1].Content[0].Text != "first reply" {
		t.Errorf("after Undo, first turn should remain intact; got %+v", out)
	}
}

func TestUndo_StripsToolLoopBundle(t *testing.T) {
	// User asks; assistant calls tool twice; final assistant reply.
	// Undo should drop everything from "second prompt" onward — including
	// all the tool back-and-forth — and leave "first reply" intact.
	msgs := []llm.Message{
		userText("first prompt"),
		assistantText("first reply"),
		userText("second prompt"),
		toolUseBlock("c1", "Read"),
		toolResultBlock("c1", "(file content)"),
		toolUseBlock("c2", "Edit"),
		toolResultBlock("c2", "(ok)"),
		assistantText("done"),
	}
	out, ok := Undo(msgs)
	if !ok {
		t.Fatal("Undo should succeed even when a tool loop sits in the last turn")
	}
	if len(out) != 2 {
		t.Fatalf("Undo result len = %d, want 2", len(out))
	}
	if out[1].Content[0].Text != "first reply" {
		t.Errorf("first reply should remain after Undo; got %+v", out)
	}
}

func TestUndo_OnEmptyHistoryIsNoop(t *testing.T) {
	out, ok := Undo(nil)
	if ok {
		t.Error("Undo on empty history should report ok=false")
	}
	if len(out) != 0 {
		t.Errorf("Undo on empty should return empty; got %+v", out)
	}
}

func TestUndo_OnAssistantOnlyHistoryIsNoop(t *testing.T) {
	msgs := []llm.Message{assistantText("first")}
	out, ok := Undo(msgs)
	if ok {
		t.Error("Undo with no plain-text user message should report ok=false")
	}
	if len(out) != 1 {
		t.Errorf("history should be unchanged; got %+v", out)
	}
}

func TestLastPlainUserIndex_IgnoresToolResultUserMessages(t *testing.T) {
	msgs := []llm.Message{
		userText("hi"),               // index 0 — real user msg
		assistantText("..."),         // 1
		toolUseBlock("c1", "Read"),   // 2 — assistant tool_use
		toolResultBlock("c1", "..."), // 3 — user-role tool_result
	}
	idx := LastPlainUserIndex(msgs)
	if idx != 0 {
		t.Errorf("LastPlainUserIndex = %d, want 0 (the only plain-text user msg)", idx)
	}
}

func TestSnapshot_IsDefensiveCopy(t *testing.T) {
	src := []llm.Message{userText("hi"), assistantText("ok")}
	snap := Snapshot(src)
	snap[0] = userText("MUTATED")
	if src[0].Content[0].Text != "hi" {
		t.Error("Snapshot should be a defensive copy, but src was mutated")
	}
}

func TestCountTurns(t *testing.T) {
	msgs := []llm.Message{
		userText("u1"),
		assistantText("a1"),
		userText("u2"),
		toolUseBlock("c1", "Read"),
		toolResultBlock("c1", "result"),
		assistantText("a2"),
		userText("u3"),
		assistantText("a3"),
	}
	if got := CountTurns(msgs); got != 3 {
		t.Errorf("CountTurns = %d, want 3 (tool_result user msgs don't count)", got)
	}
}
