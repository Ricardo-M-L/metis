package tui

// keybind_permission.go — permission-prompt key handling and the
// shift+tab mode-cycle (which also gates this screen). Permission
// prompts block all other input until the user picks Yes/Always/No/Cancel.

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// modeCycleStartupGrace is the window after TUI launch during which we
// ignore Shift+Tab. Bubbletea reads the alt-screen-init responses from
// some terminals as a burst of Shift+Tab keystrokes — without this gate,
// users see five "mode: …" lines before they even press a key.
const modeCycleStartupGrace = 800 * time.Millisecond

// modeCycleDebounce keeps a single human keypress from counting twice
// when the terminal sends both KeyShiftTab and a paired escape.
const modeCycleDebounce = 200 * time.Millisecond

// permissionTimeout is the wall-clock window the user has to answer a
// Yes/Always/No/Cancel prompt. After this elapses, executePermission is
// invoked with "n" — the safest default when the operator isn't around.
//
// Sized to be long enough that a returning user almost always still has
// the prompt waiting, but short enough that an automated/headless run
// cannot stall an agent indefinitely. Cross-CLI testing exposed the
// "VNC viewer left running, agent stuck for >6 minutes on a single
// permission prompt" failure mode this guards against.
const permissionTimeout = 60 * time.Second

func (m *Model) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyLeft, tea.KeyUp:
		if m.permCursor > 0 {
			m.permCursor--
		}
	case tea.KeyRight, tea.KeyDown:
		if m.permCursor < len(m.permChoices)-1 {
			m.permCursor++
		}
	case tea.KeyEnter, tea.KeySpace:
		choice := m.permChoices[m.permCursor]
		m.executePermission(choice.Key)
	case tea.KeyEscape:
		m.permActive = false
		m.executePermission("n")
	}
	return m, nil
}

// executePermission sends the user's decision through the reply
// channel and clears prompt state. The reply channel is buffered
// size 1 so the send never blocks.
func (m *Model) executePermission(key string) {
	if m.permReply != nil {
		switch key {
		case "y":
			m.permReply <- agent.PermissionDecisionAllow
		case "a":
			m.permReply <- agent.PermissionDecisionAlwaysAllow
		case "c":
			// Cancel: deny this tool AND abort the turn. The agent
			// loop sees a Deny and won't proceed; turnCancel makes
			// the streaming abort cleanly.
			m.permReply <- agent.PermissionDecisionDeny
			if m.turnCancel != nil {
				m.turnCancel()
				m.turnCancel = nil
			}
		default: // "n" and any unknown key
			m.permReply <- agent.PermissionDecisionDeny
		}
		m.permReply = nil
	}
	m.permActive = false
	m.permQuestion = ""
	m.permTool = ""
	m.permArgs = ""
}

func (m *Model) cyclePermissionMode() {
	now := time.Now()
	if now.Sub(m.startTime) < modeCycleStartupGrace {
		return
	}
	if !m.lastModeCycle.IsZero() && now.Sub(m.lastModeCycle) < modeCycleDebounce {
		return
	}
	m.lastModeCycle = now

	modes := []string{"ask", "auto", "bypass", "plan", "deny"}
	current := string(m.gate.Mode())
	nextIdx := 0
	for i, mode := range modes {
		if mode == current {
			nextIdx = (i + 1) % len(modes)
			break
		}
	}
	m.gate.SetMode(permission.Mode(modes[nextIdx]))
	// Mode change is shown in the footer ("· auto mode on (shift+tab to
	// cycle)"). We deliberately don't append a chat message — every
	// Shift+Tab press would otherwise pollute history.
}
