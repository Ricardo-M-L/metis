package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func userMsg(s string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: s}}}
}

func assistantText(s string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: s}}}
}

func assistantToolUse(name string, input map[string]any) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "c1", ToolName: name, ToolInput: input},
	}}
}

func toolResultUser(id, output string, isErr bool) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: id, ToolResult: output, IsError: isErr},
	}}
}

func TestHistoryScreen_RendersUserAndAssistant(t *testing.T) {
	s := NewHistoryScreen([]llm.Message{
		userMsg("hello"),
		assistantText("world"),
	}, 80, 24)
	view := s.View()
	if !strings.Contains(view, "user") || !strings.Contains(view, "hello") {
		t.Errorf("user message missing from view: %s", view)
	}
	if !strings.Contains(view, "assistant") || !strings.Contains(view, "world") {
		t.Errorf("assistant reply missing from view: %s", view)
	}
}

func TestHistoryScreen_RendersToolCallAndResult(t *testing.T) {
	s := NewHistoryScreen([]llm.Message{
		userMsg("read foo"),
		assistantToolUse("Read", map[string]any{"path": "/tmp/foo"}),
		toolResultUser("c1", "(200 lines of file)", false),
		assistantText("done"),
	}, 80, 24)
	view := s.View()
	if !strings.Contains(view, "tool: Read") {
		t.Errorf("tool call not rendered: %s", view)
	}
	if !strings.Contains(view, "result") {
		t.Errorf("tool result not rendered: %s", view)
	}
}

func TestHistoryScreen_RendersErrorTag(t *testing.T) {
	s := NewHistoryScreen([]llm.Message{
		userMsg("read missing"),
		assistantToolUse("Read", map[string]any{"path": "/nope"}),
		toolResultUser("c1", "no such file", true),
	}, 80, 24)
	view := s.View()
	if !strings.Contains(view, "error") {
		t.Errorf("error tag not rendered: %s", view)
	}
}

func TestHistoryScreen_EmptyShowsHint(t *testing.T) {
	s := NewHistoryScreen(nil, 80, 24)
	view := s.View()
	if !strings.Contains(view, "history is empty") {
		t.Errorf("expected empty hint, got: %s", view)
	}
}

func TestHistoryScreen_EscClosesAndSetsDone(t *testing.T) {
	s := NewHistoryScreen([]llm.Message{userMsg("x")}, 80, 24)
	if s.Done() {
		t.Fatal("Done should be false right after open")
	}
	updated, _ := s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !updated.Done() {
		t.Error("Esc should set Done=true")
	}
}

func TestHistoryScreen_QClosesAndSetsDone(t *testing.T) {
	s := NewHistoryScreen([]llm.Message{userMsg("x")}, 80, 24)
	updated, _ := s.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if !updated.Done() {
		t.Error("'q' should set Done=true")
	}
}

func TestHistoryScreen_ResizeRebuildsContent(t *testing.T) {
	s := NewHistoryScreen([]llm.Message{userMsg("x")}, 80, 24)
	// Just verify Resize doesn't panic and the screen still renders.
	s.Resize(120, 40)
	if v := s.View(); !strings.Contains(v, "session history") {
		t.Errorf("post-resize view lost title: %s", v)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("truncate long = %q", got)
	}
}

func TestWrap_BasicWordBreak(t *testing.T) {
	in := "the quick brown fox jumped over the lazy dog and kept going forever"
	out := wrap(in, 25)
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 25 {
			t.Errorf("wrap produced over-width line: %q (%d chars)", line, len(line))
		}
	}
}
