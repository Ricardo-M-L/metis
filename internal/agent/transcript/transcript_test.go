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
