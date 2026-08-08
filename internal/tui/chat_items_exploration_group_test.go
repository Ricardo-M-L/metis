package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	if len(items) != 1 {
		t.Fatalf("items = %d, want one grouped exploration item", len(items))
	}
	group, ok := items[0].(*explorationGroupItem)
	if !ok {
		t.Fatalf("item[0] = %T, want explorationGroupItem", items[0])
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

	// P0-1 (2026-08-02): the legacy expandToolOutputs toggle is gone;
	// explorationGroupItem's expand flag is now driven exclusively by
	// the construction site (always false in production). Test the
	// expand=true render path directly by constructing the item by hand.
	directGroup := &explorationGroupItem{events: m.toolEvents, expand: true}
	expanded := directGroup.Render(100)
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

func TestBuildChatItems_RecoveredErrorStaysInPlaceAcrossAssistantCommentary(t *testing.T) {
	now := time.Now()
	m := newSlashTestModel(t)
	m.messages = []Message{
		{Role: "user", Content: "query the photo database", Timestamp: now},
		{Role: "assistant", Content: "The first schema guess failed; I will inspect it and retry.", Timestamp: now.Add(2 * time.Millisecond)},
	}
	m.toolEvents = []ToolEvent{
		{
			ID: "query-failed", Kind: "result", ToolName: "Bash", IsError: true,
			Input:     map[string]any{"command": "sqlite3 wrong.db ...", "description": "Query Photos SQLite for IMG_0309"},
			Output:    "unable to open database",
			StartTime: now.Add(time.Millisecond),
		},
		{
			ID: "query-ok", Kind: "result", ToolName: "Bash",
			Input:     map[string]any{"command": "sqlite3 correct.db ...", "description": "Retry Photos SQLite query for IMG_0309"},
			Output:    "(no rows)",
			StartTime: now.Add(3 * time.Millisecond),
		},
	}

	items := m.buildChatItems()
	var group *recoveredErrorGroupItem
	groupIndex, commentaryIndex := -1, -1
	for idx, item := range items {
		switch typed := item.(type) {
		case *recoveredErrorGroupItem:
			group = typed
			groupIndex = idx
		case *messageItem:
			if strings.Contains(typed.msg.Content, "schema guess failed") {
				commentaryIndex = idx
			}
		}
	}
	if group == nil {
		t.Fatalf("later successful retry should compact the earlier error in place; items=%#v", items)
	}
	if groupIndex < 0 || commentaryIndex < 0 || groupIndex >= commentaryIndex {
		t.Fatalf("recovered row moved across assistant commentary: group=%d commentary=%d", groupIndex, commentaryIndex)
	}
	compact := stripANSI(group.Render(100))
	if !strings.Contains(compact, "1 intermediate error recovered") || strings.Contains(compact, "unable to open database") {
		t.Fatalf("unexpected compact recovered row:\n%s", compact)
	}

	// Drive the real keyboard path. The later successful Bash result is the
	// newest completed event, but it has no hidden body; Ctrl+O must target the
	// older recovered row that actually advertises inspection.
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.expandedToolID != "query-failed" {
		t.Fatalf("Ctrl+O selected %q, want historical recovered event", m.expandedToolID)
	}
	for _, item := range m.buildChatItems() {
		if expanded, ok := item.(*recoveredErrorGroupItem); ok {
			out := stripANSI(expanded.Render(100))
			if !strings.Contains(out, "unable to open database") {
				t.Fatalf("Ctrl+O mapping lost original recovered error:\n%s", out)
			}
			return
		}
	}
	t.Fatal("expanded rebuild lost recovered error group")
}

func TestBuildChatItems_UnrecoveredAndSecurityErrorsStayVisible(t *testing.T) {
	now := time.Now()
	m := newSlashTestModel(t)
	m.messages = []Message{{Role: "user", Content: "run it", Timestamp: now}}
	m.toolEvents = []ToolEvent{
		{
			ID: "unrecovered", Kind: "result", ToolName: "Bash", IsError: true,
			Input:  map[string]any{"command": "clone missing", "description": "Install ui-radar skill"},
			Output: "Failed to clone repository", StartTime: now.Add(time.Millisecond),
		},
		{
			ID: "denied", Kind: "result", ToolName: "Bash", IsError: true,
			Input:  map[string]any{"command": "unsafe", "description": "Run protected command"},
			Output: "denied by permission policy: bash-security rule #23", StartTime: now.Add(2 * time.Millisecond),
		},
		{
			ID: "later-ok", Kind: "result", ToolName: "Bash",
			Input:  map[string]any{"command": "safe", "description": "Retry protected command"},
			Output: "ok", StartTime: now.Add(3 * time.Millisecond),
		},
	}
	items := m.buildChatItems()
	for _, item := range items {
		if _, compacted := item.(*recoveredErrorGroupItem); compacted {
			t.Fatalf("unrecovered/security error was compacted as recovered: %#v", items)
		}
	}
	rendered := ""
	for _, item := range items {
		rendered += stripANSI(item.Render(100))
	}
	for _, want := range []string{"Failed to clone repository", "denied by permission policy"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("important error evidence %q disappeared:\n%s", want, rendered)
		}
	}
}

func TestBuildChatItems_PartialTimeoutCompactsWithoutLaterSuccess(t *testing.T) {
	now := time.Now()
	m := newSlashTestModel(t)
	m.messages = []Message{{Role: "user", Content: "install", Timestamp: now}}
	m.toolEvents = []ToolEvent{{
		ID: "partial", Kind: "result", ToolName: "Bash", IsError: true,
		Input:  map[string]any{"command": "npx skills add x", "description": "Install x skill"},
		Output: "Installation complete\nInstalled 1 skill\n[command exceeded timeout 30s]", StartTime: now.Add(time.Millisecond),
	}}
	items := m.buildChatItems()
	if len(items) != 2 { // user + compact recovered row
		t.Fatalf("items=%d, want user + recovered partial row", len(items))
	}
	group, ok := items[1].(*recoveredErrorGroupItem)
	if !ok {
		t.Fatalf("partial timeout item=%T, want recoveredErrorGroupItem", items[1])
	}
	out := stripANSI(group.Render(100))
	if !strings.Contains(out, "install reported complete before timeout; verify") {
		t.Fatalf("partial completion state missing:\n%s", out)
	}
}

func TestSameRecoveryOperation_RejectsGenericDescriptionOverlap(t *testing.T) {
	failed := ToolEvent{
		ToolName: "Bash",
		Input: map[string]any{
			"command":     "npm test",
			"description": "Run tests",
		},
	}
	succeeded := ToolEvent{
		ToolName: "Bash",
		Input: map[string]any{
			"command":     "go test ./...",
			"description": "Retry tests",
		},
	}
	if sameRecoveryOperation(failed, succeeded) {
		t.Fatal("one generic description word hid an unrelated failed command")
	}

	failed.Input["description"] = "Query Photos SQLite for IMG_0309"
	succeeded.Input["description"] = "Retry Photos SQLite query for IMG_0309"
	if !sameRecoveryOperation(failed, succeeded) {
		t.Fatal("specific multi-token retry intent should match")
	}

	failed.Input["path"] = "/tmp/a"
	succeeded.Input["path"] = "/tmp/b"
	if sameRecoveryOperation(failed, succeeded) {
		t.Fatal("concrete target mismatch must override similar descriptions")
	}

	failed.Input = map[string]any{"path": "/tmp/tree", "pattern": "TODO", "description": "Search project markers"}
	succeeded.Input = map[string]any{"path": "/tmp/tree", "pattern": "FIXME", "description": "Search project markers"}
	if sameRecoveryOperation(failed, succeeded) {
		t.Fatal("matching path must not hide a later structured pattern mismatch")
	}
}

func TestSameRecoveryOperation_DispatchActionsMustMatch(t *testing.T) {
	failed := ToolEvent{
		ToolName: "Skill",
		Input:    map[string]any{"action": "invoke", "name": "anti-ui-slop"},
	}
	succeeded := ToolEvent{
		ToolName: "Skill",
		Input:    map[string]any{"action": "get", "name": "anti-ui-slop"},
	}
	if sameRecoveryOperation(failed, succeeded) {
		t.Fatal("successful Skill get must not hide a failed Skill invoke")
	}
	succeeded.Input["action"] = "invoke"
	if !sameRecoveryOperation(failed, succeeded) {
		t.Fatal("same Skill action and target should count as a successful retry")
	}
	delete(succeeded.Input, "action")
	if sameRecoveryOperation(failed, succeeded) {
		t.Fatal("missing dispatcher action must fail closed")
	}
}
