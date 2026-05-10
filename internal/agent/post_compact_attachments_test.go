package agent

// post_compact_attachments_test.go — locks the recent-file extraction
// + synthetic message rendering used by Compact().

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func toolUseWithPath(id, name, path string) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "calling " + name},
			{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: map[string]any{"path": path}},
		},
	}
}

func TestExtractRecentToolInputPaths_NewestFirstDistinct(t *testing.T) {
	msgs := []llm.Message{
		toolUseWithPath("t1", "Read", "old.go"),
		toolResultMsg("t1", "ok"),
		toolUseWithPath("t2", "Read", "middle.go"),
		toolResultMsg("t2", "ok"),
		toolUseWithPath("t3", "Edit", "new.go"),
		toolResultMsg("t3", "ok"),
		toolUseWithPath("t4", "Read", "middle.go"), // duplicate path, newer
		toolResultMsg("t4", "ok"),
	}
	got := extractRecentToolInputPaths(msgs, 5)
	want := []string{"middle.go", "new.go", "old.go"}
	if len(got) != len(want) {
		t.Fatalf("paths len: got %d want %d (%v)", len(got), len(want), got)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("paths[%d]: got %q, want %q (full=%v)", i, got[i], p, got)
		}
	}
}

func TestExtractRecentToolInputPaths_HonorsCap(t *testing.T) {
	var msgs []llm.Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, toolUseWithPath(string(rune('a'+i)), "Read", string(rune('a'+i))+".go"))
	}
	got := extractRecentToolInputPaths(msgs, 3)
	if len(got) != 3 {
		t.Errorf("cap=3 not honored; got %d entries", len(got))
	}
}

func TestExtractRecentToolInputPaths_SkipsNonFileTools(t *testing.T) {
	msgs := []llm.Message{
		toolUseWithPath("t1", "Glob", "**/*.go"), // pattern, not a path
		toolUseWithPath("t2", "Bash", "/bin/ls"), // not in the set
		toolUseWithPath("t3", "Read", "real.go"),
	}
	got := extractRecentToolInputPaths(msgs, 5)
	if len(got) != 1 || got[0] != "real.go" {
		t.Errorf("non-file tools should be skipped; got %v", got)
	}
}

func TestBuildPostCompactAttachment_EmptyWhenNoFiles(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "hi"),
		msg(llm.RoleAssistant, "hello"),
	}
	att := BuildPostCompactAttachment(msgs)
	if len(att.Content) != 0 {
		t.Errorf("expected empty attachment when no Read/Edit/Write happened; got %+v", att)
	}
}

func TestBuildPostCompactAttachment_WrapsInPostCompactContextEnvelope(t *testing.T) {
	msgs := []llm.Message{
		toolUseWithPath("t1", "Read", "internal/foo.go"),
		toolUseWithPath("t2", "Edit", "internal/bar.go"),
	}
	att := BuildPostCompactAttachment(msgs)
	if len(att.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(att.Content))
	}
	body := att.Content[0].Text
	if !strings.HasPrefix(body, "<post_compact_context>") {
		t.Errorf("missing envelope prefix: %q", body)
	}
	if !strings.HasSuffix(body, "</post_compact_context>") {
		t.Errorf("missing envelope suffix: %q", body)
	}
	if !strings.Contains(body, "internal/foo.go") || !strings.Contains(body, "internal/bar.go") {
		t.Errorf("attachment body missing path entries: %q", body)
	}
	if att.Role != llm.RoleUser {
		t.Errorf("attachment must use user role to satisfy alternation; got %q", att.Role)
	}
}
