package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func navigationBash(id string, at time.Time) ToolEvent {
	return ToolEvent{
		ID: id, Kind: "end", ToolName: "Bash", StartTime: at,
		Input:  map[string]any{"command": "printf '" + id + " evidence'", "description": "Inspect " + id},
		Output: id + " evidence",
	}
}

func navigationTranscript(m *Model, width int) string {
	var out strings.Builder
	for _, item := range m.buildChatItems() {
		out.WriteString(item.Render(width))
		out.WriteByte('\n')
	}
	return stripANSI(out.String())
}

func TestToolNavigationToggleAndSelectionDoNotChangeHistory(t *testing.T) {
	m := newSlashTestModel(t)
	now := time.Now()
	m.messages = []Message{{ID: "original", Role: "user", Content: "Inspect the files", Timestamp: now}}
	m.toolEvents = []ToolEvent{
		navigationBash("old", now.Add(time.Millisecond)),
		navigationBash("middle", now.Add(2*time.Millisecond)),
		navigationBash("latest", now.Add(3*time.Millisecond)),
	}
	m.input.SetValue("未提交草稿\nsecond line")
	originalMessages := append([]Message(nil), m.messages...)
	originalEvents := append([]ToolEvent(nil), m.toolEvents...)
	steps := []struct {
		key      tea.KeyPressMsg
		selected string
		expanded string
	}{
		{tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, "latest", "latest"},
		{tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, "latest", ""},
		{tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}, "middle", ""},
		{tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, "middle", "middle"},
		{tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}, "old", "old"},
		{tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}, "old", "old"},
		{tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt}, "middle", "middle"},
		{tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt}, "latest", "latest"},
		{tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt}, "latest", "latest"},
	}
	for index, step := range steps {
		drive(t, m, step.key)
		if m.selectedToolID != step.selected || m.expandedToolID != step.expanded {
			t.Fatalf("step %d: selected=%q expanded=%q, want %q/%q", index, m.selectedToolID, m.expandedToolID, step.selected, step.expanded)
		}
		out := navigationTranscript(m, 98)
		action := "(ctrl+O to expand)"
		if step.expanded != "" {
			action = "(ctrl+O to collapse)"
		}
		if strings.Count(out, "Selected tool") != 1 || strings.Count(out, action) != 1 {
			t.Fatalf("step %d: selection/action must appear exactly once:\n%s", index, out)
		}
		if !reflect.DeepEqual(m.messages, originalMessages) || !reflect.DeepEqual(m.toolEvents, originalEvents) {
			t.Fatalf("step %d: keyboard navigation changed the transcript", index)
		}
		if m.input.Value() != "未提交草稿\nsecond line" {
			t.Fatalf("step %d: keyboard navigation changed the draft: %q", index, m.input.Value())
		}
	}
}

func TestToolNavigationKeepsExplicitSelectionWhenResultsArrive(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = nil
	now := time.Now()
	m.toolEvents = []ToolEvent{navigationBash("old", now), navigationBash("latest", now.Add(time.Millisecond))}
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	if m.selectedToolID != "old" || m.expandedToolID != "" {
		t.Fatalf("initial Alt+Up should select the previous collapsed tool: %q/%q", m.selectedToolID, m.expandedToolID)
	}
	m.toolEvents = append(m.toolEvents, navigationBash("arrived", now.Add(2*time.Millisecond)))
	_ = m.buildChatItems()
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.selectedToolID != "old" || m.expandedToolID != "old" {
		t.Fatalf("new results moved the selected tool: %q/%q", m.selectedToolID, m.expandedToolID)
	}
	m.toolEvents = m.toolEvents[1:]
	_ = m.buildChatItems()
	if m.selectedToolID != "" || m.expandedToolID != "" {
		t.Fatal("removed event retained stale selection")
	}
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.selectedToolID != "arrived" || m.expandedToolID != "arrived" {
		t.Fatalf("invalid selection did not safely fall back to latest: %q/%q", m.selectedToolID, m.expandedToolID)
	}
	m.sessionID = "another-session"
	_ = m.buildChatItems()
	if m.selectedToolID != "" || m.expandedToolID != "" {
		t.Fatal("selection leaked into another session")
	}
	m.toolEvents = nil
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if len(m.messages) != 0 || m.selectedToolID != "" {
		t.Fatal("empty navigation wrote history or selected an absent tool")
	}
}

func TestToolNavigationGroupedDetailsRemainReachable(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = nil
	now := time.Now()
	m.toolEvents = []ToolEvent{
		{ID: "read-a", Kind: "end", ToolName: "Read", Input: map[string]any{"path": "/tmp/first.go"}, Output: "first evidence", StartTime: now},
		{ID: "search-b", Kind: "end", ToolName: "Grep", Input: map[string]any{"pattern": "NEEDLE"}, Output: "match evidence", StartTime: now.Add(time.Millisecond)},
		navigationBash("latest", now.Add(2*time.Millisecond)),
	}
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	if m.selectedToolID != "search-b" || m.expandedToolID != "" {
		t.Fatalf("group not selected collapsed: %q/%q", m.selectedToolID, m.expandedToolID)
	}
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	out := navigationTranscript(m, 98)
	for _, want := range []string{"first.go", "NEEDLE", "first evidence", "match evidence", "(ctrl+O to collapse)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expanded group lost %q:\n%s", want, out)
		}
	}
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.selectedToolID != "search-b" || m.expandedToolID != "" {
		t.Fatalf("second Ctrl+O did not collapse the same group: %q/%q", m.selectedToolID, m.expandedToolID)
	}
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	if m.selectedToolID != "latest" {
		t.Fatalf("next tool not reachable from group: %q", m.selectedToolID)
	}
}

func TestToolNavigationScrollsToSelectedBlockStart(t *testing.T) {
	m := newE2EModel(t, 100, 24, 40)
	old := navigationBash("old", m.messages[5].Timestamp.Add(time.Microsecond))
	var rows []string
	for i := 0; i < 60; i++ {
		rows = append(rows, fmt.Sprintf("evidence row %02d", i))
	}
	old.Output = strings.Join(rows, "\n")
	m.toolEvents = []ToolEvent{old, navigationBash("latest", m.messages[39].Timestamp.Add(time.Millisecond))}
	_ = m.View()
	m.chatList.ScrollToBottom()
	drive(t, m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	for frame := 0; frame < 2; frame++ {
		out := stripANSI(m.View().Content)
		for _, want := range []string{"Selected tool", "Inspect old", "evidence row 00", "(ctrl+O to collapse)"} {
			if !strings.Contains(out, want) {
				t.Fatalf("frame %d: selected block start %q not visible:\n%s", frame, want, out)
			}
		}
	}
	if m.stickyBottom {
		t.Fatal("tool navigation retained sticky-bottom scrolling")
	}
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "Selected tool") || !strings.Contains(out, "Inspect old") || m.selectedToolID != "old" {
		t.Fatalf("collapse lost selected block visibility:\n%s", out)
	}
}

func TestToolNavigationHintVisibilityAndEvidence(t *testing.T) {
	for _, width := range []int{24, 40, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newSlashTestModel(t)
			m.messages = nil
			m.width = width + 2
			m.toolEvents = []ToolEvent{navigationBash("old", time.Now())}
			if out := navigationTranscript(m, width); !strings.Contains(out, "Alt+↑/↓") {
				t.Fatalf("unselected tool lacks discoverable navigation:\n%s", out)
			}
			drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
			out := stripANSI(clampBlock(navigationTranscript(m, width), width))
			if strings.Count(out, "(ctrl+O to collapse)") != 1 || strings.Count(out, "Selected tool") != 1 {
				t.Fatalf("narrow selection shortcut clipped/duplicated:\n%s", out)
			}
		})
	}
	m := newSlashTestModel(t)
	m.messages = nil
	m.toolEvents = []ToolEvent{navigationBash("literal", time.Now())}
	m.toolEvents[0].Output = "raw output says (ctrl+O to expand)"
	drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if out := navigationTranscript(m, 98); !strings.Contains(out, m.toolEvents[0].Output) {
		t.Fatalf("UI hint adaptation changed literal command output:\n%s", out)
	}
}
