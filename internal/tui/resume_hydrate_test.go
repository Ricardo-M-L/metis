package tui

// resume_hydrate_test.go pins the 2026-05-15 fix for `--resume <id>`
// opening into a blank chat surface. See resume_hydrate.go for the
// architectural why (two stores, one disk-restored, the other not).

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// makeLoopWithHistory returns a *agent.Loop with the supplied messages
// pre-loaded — what ApplyResume produces. We don't need any of the
// loop's other plumbing (provider / registry) since hydrate just reads
// History().
func makeLoopWithHistory(t *testing.T, msgs []llm.Message) *agent.Loop {
	t.Helper()
	l := &agent.Loop{Messages: msgs}
	return l
}

func TestHydrateFromLoopHistory_FreshSessionIsNoOp(t *testing.T) {
	t.Parallel()
	m := &Model{loop: &agent.Loop{}, sessionID: "fresh"}
	m.hydrateFromLoopHistory()
	if len(m.messages) != 0 {
		t.Errorf("fresh session should hydrate nothing; got %d messages", len(m.messages))
	}
	if len(m.toolEvents) != 0 {
		t.Errorf("fresh session should hydrate nothing; got %d toolEvents", len(m.toolEvents))
	}
}

func TestHydrateFromLoopHistory_NilLoopIsNoOp(t *testing.T) {
	t.Parallel()
	// Defensive: hydrate must NOT panic when called on a Model whose
	// loop pointer is nil (test-construction edge).
	m := &Model{loop: nil, sessionID: "x"}
	m.hydrateFromLoopHistory()
	if len(m.messages) != 0 || len(m.toolEvents) != 0 {
		t.Errorf("nil loop should yield no items")
	}
}

func TestHydrateFromLoopHistory_TextMessages(t *testing.T) {
	t.Parallel()
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "what's 2+2?"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "4"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "and 3+3?"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "6"}}},
	}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "abc12345-rest"}
	m.hydrateFromLoopHistory()

	// 4 text messages + 1 trailing info marker.
	if len(m.messages) != 5 {
		t.Fatalf("expected 5 messages (4 hist + 1 marker); got %d", len(m.messages))
	}
	want := []struct {
		role    string
		content string
	}{
		{"user", "what's 2+2?"},
		{"assistant", "4"},
		{"user", "and 3+3?"},
		{"assistant", "6"},
		{"info", "resumed"},
	}
	for i, w := range want {
		if m.messages[i].Role != w.role {
			t.Errorf("msg[%d].Role = %q, want %q", i, m.messages[i].Role, w.role)
		}
		if !strings.Contains(m.messages[i].Content, w.content) {
			t.Errorf("msg[%d].Content = %q, want substring %q", i, m.messages[i].Content, w.content)
		}
	}
	// Marker should mention message count and a short session id.
	tail := m.messages[len(m.messages)-1].Content
	if !strings.Contains(tail, "4 messages") {
		t.Errorf("marker should mention restored count; got %q", tail)
	}
	if !strings.Contains(tail, "abc12345") {
		t.Errorf("marker should include short session id; got %q", tail)
	}
}

func TestHydrateFromLoopHistory_SkipsEmptyText(t *testing.T) {
	t.Parallel()
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "text", Text: ""},          // skipped
			{Type: "text", Text: "   \n\t  "}, // skipped (whitespace)
			{Type: "text", Text: "hi"},        // kept
		}},
	}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "x"}
	m.hydrateFromLoopHistory()

	// 1 user message ("hi") + 1 marker.
	if len(m.messages) != 2 {
		t.Fatalf("expected 2 messages; got %d (%v)", len(m.messages), msgRoles(m.messages))
	}
	if m.messages[0].Content != "hi" {
		t.Errorf("expected hi; got %q", m.messages[0].Content)
	}
}

func TestHydrateFromLoopHistory_RestoresThinkingBlocks(t *testing.T) {
	t.Parallel()
	hist := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "thinking", Text: "先分析问题"},
		{Type: "redacted_thinking", Data: "OPAQUE-CIPHER"},
		{Type: "text", Text: "最终回答"},
	}}}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "reasoning-resume"}
	m.hydrateFromLoopHistory()

	if len(m.messages) != 4 {
		t.Fatalf("messages = %+v", m.messages)
	}
	wantRoles := []string{"thinking", "redacted_thinking", "assistant", "info"}
	for i, role := range wantRoles {
		if m.messages[i].Role != role {
			t.Fatalf("message %d role = %q, want %q", i, m.messages[i].Role, role)
		}
	}
	if m.messages[0].Content != "先分析问题" || m.messages[1].Content != "OPAQUE-CIPHER" {
		t.Fatalf("thinking content not restored: %+v", m.messages[:2])
	}
}

func TestHydrateFromLoopHistory_ToolUseAndResult(t *testing.T) {
	t.Parallel()
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "read /etc/hosts"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "text", Text: "I'll read that file."},
			{Type: "tool_use", ToolUseID: "tu-1", ToolName: "Read", ToolInput: map[string]any{"path": "/etc/hosts"}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu-1", ToolResult: "127.0.0.1 localhost\n", IsError: false},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "It contains 1 line: 127.0.0.1 localhost"}}},
	}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "x"}
	m.hydrateFromLoopHistory()

	// 3 text messages (user, assistant text, assistant final) + 1 marker = 4.
	if len(m.messages) != 4 {
		t.Errorf("expected 4 messages; got %d (roles=%v)", len(m.messages), msgRoles(m.messages))
	}
	// 1 ToolEvent — the Read call.
	if len(m.toolEvents) != 1 {
		t.Fatalf("expected 1 toolEvent; got %d", len(m.toolEvents))
	}
	te := m.toolEvents[0]
	if te.ToolName != "Read" {
		t.Errorf("ToolName = %q, want Read", te.ToolName)
	}
	if te.Kind != "end" {
		t.Errorf("Kind = %q, want end (tool_result block should have upgraded it)", te.Kind)
	}
	if te.Output != "127.0.0.1 localhost\n" {
		t.Errorf("Output = %q", te.Output)
	}
	if te.IsError {
		t.Error("IsError should be false")
	}
	if path, _ := te.Input["path"].(string); path != "/etc/hosts" {
		t.Errorf("Input.path = %q", path)
	}
}

func TestHydrateFromLoopHistory_ToolUseWithoutResult_StaysStart(t *testing.T) {
	t.Parallel()
	// A tool call that never got a result (interrupted / killed mid-turn)
	// should leave the ToolEvent in the "start" state — renderToolEvent
	// handles that by showing the in-flight ellipsis.
	hist := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "tu-orphan", ToolName: "Bash", ToolInput: map[string]any{"command": "sleep 999"}},
		}},
	}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "x"}
	m.hydrateFromLoopHistory()
	if len(m.toolEvents) != 1 {
		t.Fatalf("expected 1 toolEvent; got %d", len(m.toolEvents))
	}
	if m.toolEvents[0].Kind != "start" {
		t.Errorf("orphan tool_use should stay Kind=start; got %q", m.toolEvents[0].Kind)
	}
}

func TestHydrateFromLoopHistory_OrphanToolResultIsDropped(t *testing.T) {
	t.Parallel()
	// A tool_result block whose tool_use we never saw (corrupt log,
	// pre-fix old format) must NOT fabricate a ToolEvent — the
	// renderer expects ToolName to be set.
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "ghost", ToolResult: "stuff"},
		}},
	}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "x"}
	m.hydrateFromLoopHistory()
	if len(m.toolEvents) != 0 {
		t.Errorf("orphan tool_result should not create ToolEvent; got %d", len(m.toolEvents))
	}
}

func TestHydrateFromLoopHistory_TimestampOrderingPreservesConversationFlow(t *testing.T) {
	t.Parallel()
	// timeline() merges m.messages + m.toolEvents and stable-sorts by
	// timestamp. Hydrated items must have monotonically-increasing
	// timestamps so the merge interleaves them in the original
	// conversation order — otherwise text+tool_use in the same
	// assistant message would render in the wrong relative position.
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "do it"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "text", Text: "starting now"},
			{Type: "tool_use", ToolUseID: "tu-1", ToolName: "Bash", ToolInput: map[string]any{"command": "pwd"}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu-1", ToolResult: "/tmp\n"},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	}
	m := &Model{loop: makeLoopWithHistory(t, hist), sessionID: "x"}
	m.hydrateFromLoopHistory()

	tl := m.timeline()
	// Expected order: user "do it" → asst "starting now" → tool start
	// → asst "done" (note: tool_result block updates the existing
	// toolEvent in place, doesn't insert a new timeline item) →
	// info marker.
	if len(tl) != 5 {
		t.Fatalf("expected 5 timeline items; got %d", len(tl))
	}
	gotKinds := make([]string, len(tl))
	for i, item := range tl {
		switch {
		case item.msg != nil:
			gotKinds[i] = item.msg.Role
		case item.te != nil:
			gotKinds[i] = "tool:" + item.te.ToolName
		}
	}
	wantKinds := []string{"user", "assistant", "tool:Bash", "assistant", "info"}
	for i, w := range wantKinds {
		if gotKinds[i] != w {
			t.Errorf("timeline[%d] = %q, want %q (full: %v)", i, gotKinds[i], w, gotKinds)
		}
	}
}

// msgRoles is a small debug helper for failure messages.
func msgRoles(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}
