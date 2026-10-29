package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"
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
		action := "ctrl+O expand"
		if step.expanded != "" {
			action = "ctrl+O collapse"
		}
		if strings.Count(out, "› "+glyphBullet+" ") != 1 || strings.Contains(out, "Selected tool") || strings.Count(out, action) != 1 {
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
	for _, want := range []string{"first.go", "NEEDLE", "first evidence", "match evidence", "ctrl+O collapse"} {
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
		for _, want := range []string{"› ", "Inspect old", "evidence row 00", "ctrl+O collapse"} {
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
	if !strings.Contains(out, "› ") || !strings.Contains(out, "Inspect old") || m.selectedToolID != "old" {
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
			if out := navigationTranscript(m, width); strings.Contains(out, "Alt+↑/↓") || strings.Contains(out, "ctrl+O") {
				t.Fatalf("unselected tool should not repeat keyboard hints:\n%s", out)
			}
			drive(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
			out := stripANSI(clampBlock(navigationTranscript(m, width), width))
			if strings.Count(out, "ctrl+O collapse") != 1 || strings.Count(out, "› ") != 1 || strings.Contains(out, "Selected tool") {
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

func TestToolNavigationCompactLeaderPreservesStatusAndEvidence(t *testing.T) {
	for _, width := range []int{24, 40, 80} {
		for _, state := range []string{"success", "error", "denied", "subagent"} {
			t.Run(fmt.Sprintf("%d/%s", width, state), func(t *testing.T) {
				te := navigationBash("short", time.Now())
				te.Input["command"] = "echo evidence"
				te.Output = "evidence line"
				statusStyle := styleSuccess
				switch state {
				case "error":
					te.IsError = true
					statusStyle = styleErr
				case "denied":
					te.IsError = true
					te.Output = "denied: evidence line"
					statusStyle = styleErr
				case "subagent":
					te.SubAgentParentID = "parent"
					te.ToolName = "sub: Bash"
				}
				base := renderToolEventAtWidth(te, true, width)
				selected := (toolNavigationVisual{enabled: true, selected: true, expanded: true}).render(base, width)
				beforeHeader := strings.Split(stripANSI(base), "\n")[0]
				afterHeader := strings.Split(stripANSI(selected), "\n")[0]
				padding := len(beforeHeader) - len(strings.TrimLeft(beforeHeader, " "))
				wantHeader := strings.Repeat(" ", padding-2) + "› " + strings.TrimLeft(beforeHeader, " ")
				if afterHeader != wantHeader || xansi.StringWidth(afterHeader) != xansi.StringWidth(beforeHeader) {
					t.Fatalf("selection must use the leader's existing padding: before=%q after=%q", beforeHeader, afterHeader)
				}
				if !strings.Contains(selected, statusStyle.Render(glyphBullet+" ")) {
					t.Fatalf("selection changed the tool status colour:\n%q", selected)
				}
				if !strings.Contains(selected, styleAccent.Render("› ")) {
					t.Fatalf("selected leader lacks the accent marker:\n%q", selected)
				}
				paddedAction := strings.Repeat(" ", min(padding+5, width-15)) + "ctrl+O collapse"
				if !strings.Contains(selected, styleMuted.Render(" · ctrl+O collapse")) && !strings.Contains(selected, styleMuted.Render(paddedAction)) {
					t.Fatalf("selected key hint must be low contrast:\n%q", selected)
				}
				plain := stripANSI(selected)
				if strings.Contains(plain, "Selected tool") || strings.Count(plain, "ctrl+O collapse") != 1 || strings.Count(plain, "Alt+↑/↓") != 1 {
					t.Fatalf("selection should have one compact hint and no heading:\n%s", plain)
				}
				for _, line := range strings.Split(selected, "\n") {
					if xansi.StringWidth(line) > width {
						t.Fatalf("selected row exceeds width %d: %q", width, stripANSI(line))
					}
				}
				if !strings.Contains(plain, "evidence line") {
					t.Fatalf("selection hid the original output:\n%s", plain)
				}
			})
		}
	}
}

func TestToolNavigationCompactEmptySuccessUsesAuxiliaryRow(t *testing.T) {
	for _, width := range []int{40, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			te := navigationBash("empty", time.Now())
			te.Output = ""
			base := renderToolEventAtWidth(te, false, width)
			visual := toolNavigationVisual{enabled: true, selected: true}
			selected := stripANSI(visual.render(base, width))
			rows := strings.Split(strings.TrimRight(selected, "\n"), "\n")
			if len(rows) != 2 || !strings.Contains(rows[1], glyphTreeLeaf) || !strings.Contains(rows[1], "ctrl+O expand") || !strings.Contains(rows[1], "Alt+↑/↓") {
				t.Fatalf("empty success should keep controls on its existing auxiliary row:\n%s", selected)
			}
		})
	}
}

func TestToolNavigationDoesNotTreatOutputAsAuxiliaryChrome(t *testing.T) {
	raw := "    " + glyphTreeLeaf + " raw output (ctrl+O to expand)"
	body := styleSuccess.Render("  "+glyphBullet+" ") + "Bash\n" + raw + "\n"
	selected := (toolNavigationVisual{enabled: true, selected: true}).render(body, 80)
	if !strings.Contains(selected, raw+"\n") {
		t.Fatalf("selection modified raw output that resembles UI chrome:\n%q", selected)
	}
}

func TestToolNavigationKeepsWrappedFailureSummaryContiguous(t *testing.T) {
	for _, width := range []int{24, 40} {
		for _, nested := range []bool{false, true} {
			for _, expanded := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/nested=%t/expanded=%t", width, nested, expanded), func(t *testing.T) {
					te := navigationBash("failed", time.Now())
					te.IsError, te.Output = true, ""
					if nested {
						te.SubAgentParentID, te.ToolName = "parent", "sub: Bash"
					}
					base := renderToolEventAtWidth(te, expanded, width)
					selected := (toolNavigationVisual{enabled: true, selected: true, expanded: expanded}).render(base, width)
					visible := stripANSI(clampBlock(selected, width))
					prose := strings.Join(strings.Fields(visible), " ")
					if !strings.Contains(prose, "✗ 0ms · Failed (no diagnostic output)") {
						t.Fatalf("selected shortcuts interrupted the failure explanation:\n%s", visible)
					}
					// The entire status/body suffix is copied without inserting
					// controls into either wrapped UI text or raw output.
					base = toolNavigationHints(base)
					rows := strings.Split(base, "\n")
					for index, row := range rows {
						if toolNavigationAuxiliaryRow(row) {
							if !strings.HasSuffix(selected, strings.Join(rows[index:], "\n")) {
								t.Fatalf("selection split the failure status/body suffix:\n%s", visible)
							}
							break
						}
					}
				})
			}
		}
	}
}
