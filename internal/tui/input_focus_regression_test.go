package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestPaletteEscapePreservesDraftAndStaysDismissed(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/mo")
	m.input.CursorEnd()
	m.showPalette = true
	m.palFilter = "mo"
	m.matchCommands()
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.input.Value() != "/mo" || m.paletteVisible() {
		t.Fatalf("Escape lost the draft or left the menu open: input=%q visible=%v", m.input.Value(), m.paletteVisible())
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.paletteVisible() {
		t.Fatal("cursor-only movement reopened a dismissed palette")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	m.handleKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if m.input.Value() != "/mod" || !m.paletteVisible() {
		t.Fatalf("editing the draft did not resume completion: %q", m.input.Value())
	}
}

func TestPermissionTypingSpacesCannotApprove(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}, {Label: "No", Key: "n"}}
	reply := make(chan agent.PermissionDecision, 1)
	m.handleAgentEvent(agent.Event{Kind: agent.EventPermissionRequest, ToolName: "Write", PermissionReply: reply})
	for _, r := range "do not run yet" {
		m.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if !m.permActive || m.input.Value() != "do not run yet" {
		t.Fatalf("typing answered approval or changed the draft: active=%v input=%q", m.permActive, m.input.Value())
	}
	select {
	case decision := <-reply:
		t.Fatalf("typing sent permission decision %v", decision)
	default:
	}
	if view := stripANSI(renderPermission(m)); !strings.Contains(view, "Editing prompt") || !strings.Contains(view, "Tab") {
		t.Fatalf("editing focus is not visible: %s", view)
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case decision := <-reply:
		if decision != agent.PermissionDecisionAllow {
			t.Fatalf("explicit approval selection returned %v", decision)
		}
	default:
		t.Fatal("returning focus to approval did not allow explicit confirmation")
	}
}

func TestPermissionSpaceAloneDoesNotConfirm(t *testing.T) {
	m := newSlashTestModel(t)
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}, {Label: "No", Key: "n"}}
	reply := make(chan agent.PermissionDecision, 1)
	m.handleAgentEvent(agent.Event{Kind: agent.EventPermissionRequest, ToolName: "Write", PermissionReply: reply})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if !m.permActive || len(reply) != 0 {
		t.Fatal("a space approved the pending action")
	}
}

func TestPermissionEditorEnterDoesNotApproveAndNewPromptResetsFocus(t *testing.T) {
	m := newSlashTestModel(t)
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}, {Label: "No", Key: "n"}}
	reply := make(chan agent.PermissionDecision, 1)
	m.handleAgentEvent(agent.Event{Kind: agent.EventPermissionRequest, ToolName: "Write", PermissionReply: reply})
	m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if _, _, handled := m.handlePermKey(tea.KeyPressMsg{Code: tea.KeyEnter}); handled || len(reply) != 0 {
		t.Fatal("editor Enter was treated as permission confirmation")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if decision := <-reply; decision != agent.PermissionDecisionDeny {
		t.Fatalf("Escape returned %v", decision)
	}
	next := make(chan agent.PermissionDecision, 1)
	m.handleAgentEvent(agent.Event{Kind: agent.EventPermissionRequest, ToolName: "Write", PermissionReply: next})
	if view := stripANSI(renderPermission(m)); !strings.Contains(view, "Approval focused") {
		t.Fatal("a new request retained the previous editor focus")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if decision := <-next; decision != agent.PermissionDecisionAllow {
		t.Fatalf("explicit new-request approval returned %v", decision)
	}
}

func TestPermissionEditorFocusAllowsPasteWithoutApproval(t *testing.T) {
	m := newSlashTestModel(t)
	m.permChoices = []permChoice{{Label: "Yes", Key: "y"}, {Label: "No", Key: "n"}}
	reply := make(chan agent.PermissionDecision, 1)
	m.handleAgentEvent(agent.Event{Kind: agent.EventPermissionRequest, ToolName: "Write", PermissionReply: reply})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.PasteMsg{Content: "draft with spaces"})
	if m.input.Value() != "draft with spaces" || !m.permActive || len(reply) != 0 {
		t.Fatalf("editor paste did not remain an unapproved draft: %q active=%v", m.input.Value(), m.permActive)
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if decision := <-reply; decision != agent.PermissionDecisionDeny {
		t.Fatalf("Escape returned %v, want deny", decision)
	}
}

func TestPermissionEditingShortcutsCannotLeaveApprovalFocused(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: tea.KeyEnter, Mod: tea.ModAlt},
		{Code: 'j', Mod: tea.ModCtrl},
		{Code: 'v', Mod: tea.ModCtrl},
		{Code: 'g', Mod: tea.ModCtrl},
	} {
		t.Run(key.String(), func(t *testing.T) {
			m := newSlashTestModel(t)
			reply := make(chan agent.PermissionDecision, 1)
			m.handleAgentEvent(agent.Event{Kind: agent.EventPermissionRequest, ToolName: "Write", PermissionReply: reply})
			if _, _, handled := m.handlePermKey(key); handled || !m.permInputFocused || len(reply) != 0 {
				t.Fatalf("editor shortcut retained approval focus: handled=%v editor=%v decisions=%d", handled, m.permInputFocused, len(reply))
			}
			if _, _, handled := m.handlePermKey(tea.KeyPressMsg{Code: tea.KeyEnter}); handled || len(reply) != 0 {
				t.Fatal("Enter after an editing shortcut approved the pending action")
			}
		})
	}
}
