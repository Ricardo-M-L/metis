package tui

import (
	"strings"
	"testing"
	"time"
)

func TestBuildChatItems_GroupsConsecutiveExplorationTools(t *testing.T) {
	now := time.Now()
	m := newSlashTestModel(t)
	m.messages = nil
	m.toolEvents = []ToolEvent{
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/a.go"}, Output: "a", StartTime: now},
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/b.go"}, Output: "b", StartTime: now.Add(time.Millisecond)},
		{Kind: "result", ToolName: "Grep", Input: map[string]any{"pattern": "TODO"}, Output: "hit", StartTime: now.Add(2 * time.Millisecond)},
		{Kind: "result", ToolName: "Glob", Input: map[string]any{"pattern": "**/*.go"}, Output: "/tmp/a.go", StartTime: now.Add(3 * time.Millisecond)},
	}
	items := m.buildChatItems()
	if len(items) != 2 { // welcome banner + one grouped exploration item
		t.Fatalf("items = %d, want banner + group", len(items))
	}
	group, ok := items[1].(*explorationGroupItem)
	if !ok {
		t.Fatalf("item[1] = %T, want explorationGroupItem", items[1])
	}
	compact := group.Render(100)
	for _, want := range []string{"Read 2 files", "Searched 2 patterns", "ctrl+O to expand"} {
		if !strings.Contains(compact, want) {
			t.Errorf("compact group missing %q:\n%s", want, compact)
		}
	}
	if strings.Contains(compact, "Listed") {
		t.Errorf("Glob is a search and must not be described as a directory listing:\n%s", compact)
	}
	if strings.Contains(compact, "/tmp/a.go") || strings.Contains(compact, "/tmp/b.go") {
		t.Errorf("compact group leaked per-file rows:\n%s", compact)
	}

	m.expandToolOutputs = true
	expandedItems := m.buildChatItems()
	expanded := expandedItems[1].Render(100)
	for _, want := range []string{"a.go", "b.go", "TODO", "**/*.go"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded group missing original %q:\n%s", want, expanded)
		}
	}
}

func TestBuildChatItems_ErrorAndNonExplorationBreakGroups(t *testing.T) {
	now := time.Now()
	m := newSlashTestModel(t)
	m.messages = nil
	m.toolEvents = []ToolEvent{
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/a"}, StartTime: now},
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/missing"}, IsError: true, Output: "missing", StartTime: now.Add(time.Millisecond)},
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "/tmp/b"}, StartTime: now.Add(2 * time.Millisecond)},
		{Kind: "result", ToolName: "Edit", Input: map[string]any{}, StartTime: now.Add(3 * time.Millisecond)},
		{Kind: "result", ToolName: "Grep", Input: map[string]any{"pattern": "x"}, StartTime: now.Add(4 * time.Millisecond)},
	}
	items := m.buildChatItems()
	for _, item := range items {
		if _, grouped := item.(*explorationGroupItem); grouped {
			t.Fatalf("error/Edit boundaries must prevent cross-boundary grouping: %#v", items)
		}
	}
}
