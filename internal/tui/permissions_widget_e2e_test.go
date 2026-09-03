package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
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
	m.gate.SetMode(permission.ModeAcceptEdits)
	m.input.SetValue("/permissions")
	pressEnter(t, m)

	// Cursor seeded at acceptEdits (index 1). Right once → plan (index 2).
	// Order: ask → acceptEdits → plan → bypass → deny.
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

func TestPermissionsWidget_FullAccessShowsDangerWarning(t *testing.T) {
	m := newSlashTestModel(t)
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	m.ext.Sandbox = manager
	m.gate.SetMode(permission.ModeDefault)

	picker := screen.NewPermissionsScreen(string(permission.ModeDefault), nil)
	for range 5 { // default -> acceptEdits -> plan -> dontAsk -> bypass -> fullAccess
		_, _ = picker.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	_, _ = picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if picker.Done() {
		t.Fatal("first Enter must only arm the fullAccess confirmation")
	}
	_, _ = picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(picker)

	if m.gate.Mode() != permission.ModeFullAccess {
		t.Fatalf("permission mode = %q, want fullAccess", m.gate.Mode())
	}
	if !manager.State().FullAccessRequired {
		t.Fatal("fullAccess did not force the runtime sandbox posture off")
	}
	found := false
	for _, msg := range m.messages {
		if msg.Role == "warning" && strings.Contains(msg.Content, "DANGER: fullAccess enabled") && strings.Contains(msg.Content, "process sandbox") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected prominent fullAccess warning; messages=%+v", messageContents(m))
	}
}

func TestFullAccessSlashCommandsRequireConfirmation(t *testing.T) {
	for _, input := range []string{"/fullAccess", "/mode fullAccess"} {
		t.Run(input, func(t *testing.T) {
			m := newSlashTestModel(t)
			manager, err := sandbox.NewManager(string(sandbox.ModeOff))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			m.ext.Sandbox = manager
			m.gate.SetMode(permission.ModeDefault)

			m.input.SetValue(input)
			pressEnter(t, m)

			if got := m.gate.Mode(); got != permission.ModeDefault {
				t.Fatalf("single submission changed permission mode to %q", got)
			}
			if manager.State().FullAccessRequired {
				t.Fatal("single submission disabled the process sandbox")
			}
			picker, ok := m.activeScreen.(*screen.PermissionsScreen)
			if !ok {
				t.Fatalf("single submission should open the shared fullAccess confirmation; got %T", m.activeScreen)
			}
			view := stripANSI(picker.View())
			if !strings.Contains(view, "DANGER:") || !strings.Contains(view, "Enter confirm fullAccess") {
				t.Fatalf("fullAccess confirmation is missing the danger prompt:\n%s", view)
			}

			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			*m = *(updated.(*Model))
			if got := m.gate.Mode(); got != permission.ModeFullAccess {
				t.Fatalf("confirmed permission mode = %q, want fullAccess", got)
			}
			if !manager.State().FullAccessRequired {
				t.Fatal("confirmed fullAccess did not disable the process sandbox")
			}
		})
	}
}

func TestFullAccessSlashCanConfirmWhileTurnIsActive(t *testing.T) {
	m := newSlashTestModel(t)
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	m.ext.Sandbox = manager
	m.gate.SetMode(permission.ModeDefault)
	m.turnActive = true
	m.input.SetValue("/fullAccess")

	if cmd := pressEnter(t, m); cmd != nil {
		t.Fatal("opening the mid-turn fullAccess confirmation unexpectedly started background work")
	}
	if _, ok := m.activeScreen.(*screen.PermissionsScreen); !ok {
		t.Fatalf("mid-turn /fullAccess did not open confirmation; got %T", m.activeScreen)
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	*m = *(updated.(*Model))
	if cmd == nil {
		t.Fatal("confirming mid-turn fullAccess did not queue a permission transition")
	}
	updated, _ = m.Update(runCmd(t, cmd))
	*m = *(updated.(*Model))
	if got := m.gate.Mode(); got != permission.ModeFullAccess {
		t.Fatalf("confirmed mid-turn permission mode = %q, want fullAccess", got)
	}
	if !manager.State().FullAccessRequired {
		t.Fatal("confirmed mid-turn fullAccess did not disable the process sandbox")
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("mid-turn /fullAccess leaked into model steering: %q", got)
	}
}

func TestModeCommandLeavesFullAccessWithoutDangerConfirmation(t *testing.T) {
	m := newSlashTestModel(t)
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	m.ext.Sandbox = manager
	if err := applyModelPermissionMode(m, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}

	m.input.SetValue("/mode default")
	pressEnter(t, m)

	if got := m.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("leaving fullAccess set mode %q, want default", got)
	}
	if m.activeScreen != nil {
		t.Fatalf("leaving fullAccess unexpectedly opened confirmation screen %T", m.activeScreen)
	}
	if manager.State().FullAccessRequired {
		t.Fatal("leaving fullAccess did not restore the process sandbox boundary")
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
