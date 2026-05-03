package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// TestPermissionsWidget_SlashOpensEditor — /permissions and /perms both
// open the interactive PermissionsScreen.
func TestPermissionsWidget_SlashOpensEditor(t *testing.T) {
	for _, input := range []string{"/permissions", "/perms"} {
		t.Run(input, func(t *testing.T) {
			m := newSlashTestModel(t)
			m.input.SetValue(input)
			pressEnter(t, m)
			if _, ok := m.activeScreen.(*screen.PermissionsScreen); !ok {
				t.Errorf("%s should open PermissionsScreen; got %T", input, m.activeScreen)
			}
		})
	}
}

// TestPermissionsWidget_ApplyChangesMode — Enter on the cycled mode
// commits to gate.SetMode.
func TestPermissionsWidget_ApplyChangesMode(t *testing.T) {
	m := newSlashTestModel(t)
	m.gate.SetMode(permission.ModeAuto)
	m.input.SetValue("/permissions")
	pressEnter(t, m)

	// Cursor on auto (index 1). Right twice → plan (index 3).
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // bypass
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // plan
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.gate.Mode() != permission.ModePlan {
		t.Errorf("expected mode = plan after apply; got %q", m.gate.Mode())
	}
	// Confirmation appended as success role.
	found := false
	for _, msg := range m.messages {
		if msg.Role == "success" && strings.Contains(msg.Content, "permission mode") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected success-role 'permission mode: ...' confirmation; got: %+v", messageContents(m))
	}
}

// TestPermissionsWidget_EscPreservesMode — Esc dismisses without
// changing the gate's mode even if the cursor moved.
func TestPermissionsWidget_EscPreservesMode(t *testing.T) {
	m := newSlashTestModel(t)
	m.gate.SetMode(permission.ModeAsk)
	m.input.SetValue("/permissions")
	pressEnter(t, m)

	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if m.gate.Mode() != permission.ModeAsk {
		t.Errorf("Esc should preserve mode = ask; got %q", m.gate.Mode())
	}
}
