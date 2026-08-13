package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func contextToolUse(id, name, path string) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type:      "tool_use",
			ToolUseID: id,
			ToolName:  name,
			ToolInput: map[string]any{"path": path},
		}},
	}
}

func contextToolResult(id string, failed bool) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type:       "tool_result",
			ToolUseID:  id,
			ToolResult: "contents",
			IsError:    failed,
		}},
	}
}

func TestRenderContextFilesUsesLiveConversationNotWorkspaceIndex(t *testing.T) {
	loop := &agent.Loop{
		Messages: []llm.Message{
			contextToolUse("read-ok", "Read", "/repo/internal/loaded.go"),
			contextToolResult("read-ok", false),
			contextToolUse("read-failed", "Read", "/repo/internal/missing.go"),
			contextToolResult("read-failed", true),
			contextToolUse("glob", "Glob", "/repo/**/*.go"),
			contextToolResult("glob", false),
		},
	}

	got := renderContextFiles(&REPL{Loop: loop})
	if !strings.Contains(got, "/repo/internal/loaded.go") {
		t.Fatalf("loaded file missing:\n%s", got)
	}
	for _, unwanted := range []string{"missing.go", "**/*.go", "indexed", "showing first"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("context file output contains %q:\n%s", unwanted, got)
		}
	}
}

func TestRenderContextFilesIncludesProjectInstructionsAndDeduplicates(t *testing.T) {
	const projectFile = "/repo/CLAUDE.md"
	loop := &agent.Loop{
		SystemSections: []llm.SystemSection{{
			Name: "project",
			Body: "<project_context source=\"" + projectFile + "\">\nrules\n</project_context>",
		}},
		Messages: []llm.Message{
			contextToolUse("read-1", "Read", projectFile),
			contextToolResult("read-1", false),
			contextToolUse("read-2", "Read", "/repo/main.go"),
			contextToolResult("read-2", false),
			contextToolUse("read-3", "Read", "/repo/main.go"),
			contextToolResult("read-3", false),
		},
	}

	got := renderContextFiles(&REPL{Loop: loop})
	if strings.Count(got, projectFile) != 1 {
		t.Fatalf("project file should be listed once:\n%s", got)
	}
	if strings.Count(got, "/repo/main.go") != 1 {
		t.Fatalf("re-read file should be listed once:\n%s", got)
	}
	if !strings.Contains(got, "project instructions") || !strings.Contains(got, "read") {
		t.Fatalf("file provenance missing:\n%s", got)
	}
}

func TestRenderContextFilesEmptyState(t *testing.T) {
	got := renderContextFiles(&REPL{Loop: &agent.Loop{}})
	if !strings.Contains(got, "No files are currently loaded") {
		t.Fatalf("empty state is unclear: %q", got)
	}
}

func TestRenderContextFilesSanitizesUntrustedPathLabels(t *testing.T) {
	secret := "/repo/ghp_" + strings.Repeat("a", 36) + ".txt"
	control := "/repo/line\nbreak.go"
	escape := "/repo/\x1b[2Jspoof.go"
	long := "/repo/" + strings.Repeat("模", 80) + ".go"
	loop := &agent.Loop{Messages: []llm.Message{
		contextToolUse("secret", "Read", secret), contextToolResult("secret", false),
		contextToolUse("control", "Read", control), contextToolResult("control", false),
		contextToolUse("escape", "Read", escape), contextToolResult("escape", false),
		contextToolUse("long", "Read", long), contextToolResult("long", false),
	}}

	got := renderContextFiles(&REPL{Loop: loop})
	if strings.Contains(got, secret) || strings.Contains(got, control) ||
		strings.Contains(got, escape) || strings.Contains(got, long) {
		t.Fatalf("raw unsafe context path leaked:\n%q", got)
	}
	if strings.ContainsRune(got, '\x1b') || strings.Contains(got, "line\nbreak.go") {
		t.Fatalf("terminal control path leaked:\n%q", got)
	}
	if count := strings.Count(got, "[private]"); count != 3 {
		t.Fatalf("private path labels = %d, want 3:\n%s", count, got)
	}
	wantLong := safeArchiveLabel(long)
	if !strings.Contains(got, wantLong) || len([]rune(wantLong)) > 64 {
		t.Fatalf("bounded path label %q missing:\n%s", wantLong, got)
	}

	// Sanitization is a display boundary: internal provenance retains the
	// exact path so dedupe and future request accounting remain correct.
	entries := contextFilesFromRequest("", nil, loop.History())
	paths := make(map[string]bool, len(entries))
	for _, entry := range entries {
		paths[entry.path] = true
	}
	for _, raw := range []string{secret, control, escape, long} {
		if !paths[raw] {
			t.Errorf("internal context path was rewritten: %q", raw)
		}
	}
}

func TestCanonicalFilesUsesTheLiveContextCollector(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Messages = []llm.Message{
		contextToolUse("read-live", "Read", "/repo/live.go"),
		contextToolResult("read-live", false),
	}
	m.input.SetValue("/files")
	pressEnter(t, m)

	if m.activeScreen == nil {
		t.Fatal("/files did not open its context-files surface")
	}
	got := m.activeScreen.View()
	if !strings.Contains(got, "Files in current context") || !strings.Contains(got, "/repo/live.go") {
		t.Fatalf("canonical /files did not use live context:\n%s", got)
	}
	if strings.Contains(got, "indexed") || strings.Contains(got, "showing first") {
		t.Fatalf("canonical /files fell back to workspace index:\n%s", got)
	}
}
