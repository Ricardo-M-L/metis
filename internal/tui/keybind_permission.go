package tui

// keybind_permission.go — permission-prompt key handling and the
// shift+tab mode-cycle (which also gates this screen). Permission
// prompts block all other input until the user picks Yes/Always/No/Cancel.

import (
	"time"

	tea "charm.land/bubbletea/v2"

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
//
// 2026-05-22: changed from const → var so config.UI.PermissionTimeoutSeconds
// can override at startup. SetPermissionTimeout is the injection hook
// called by cmd/metis/main.go after config load. Tests can also poke
// it directly to exercise timeout paths in fast time.
var permissionTimeout = 60 * time.Second

// SetPermissionTimeout overrides the wall-clock window for permission
// prompts. Called once at startup from cmd/metis/main.go with the
// value from config.UI.PermissionTimeout(). Caller is responsible for
// ensuring d > 0 (a zero/negative value would auto-deny instantly).
func SetPermissionTimeout(d time.Duration) {
	if d > 0 {
		permissionTimeout = d
	}
}

// handlePermKey processes a key while the permission prompt is up.
//
// Returns handled=true ONLY when the key is part of the prompt's
// vocabulary (arrow navigation, enter/space confirm, esc deny). Every
// other key returns handled=false so the caller can forward it to the
// input editor — that's the user-feedback fix for image #18, where a
// permission prompt was locking the textarea entirely until the user
// answered. Now the user can keep composing the next prompt while a
// permission popup waits for a decision; only the keys that would
// ambiguously target both the prompt and the editor are intercepted.
//
// claude-code, crush, and opencode all block the editor entirely when
// their permission prompts are visible — metis diverges here because
// the user explicitly asked for "type while waiting" behavior. The
// risk (accidental enter-as-confirm-vs-submit) is mitigated by NOT
// listing letter shortcuts (y/n/a/c) as intercepted: the user can
// freely type "your code" without granting permission.
func (m *Model) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch msg.String() {
	case "left", "up":
		if m.permCursor > 0 {
			m.permCursor--
		}
		return m, nil, true
	case "right", "down":
		if m.permCursor < len(m.permChoices)-1 {
			m.permCursor++
		}
		return m, nil, true
	case "enter", "space":
		choice := m.permChoices[m.permCursor]
		m.executePermission(choice.Key)
		return m, nil, true
	case "esc":
		m.permActive = false
		m.executePermission("n")
		return m, nil, true
	}
	return m, nil, false
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

	// Claude Code's public cycle intentionally excludes dontAsk:
	// default -> acceptEdits -> plan -> bypassPermissions -> default.
	modes := permission.CycleModes
	current := m.gate.Mode()
	nextIdx := 0
	for i, mode := range modes {
		if mode == current {
			nextIdx = (i + 1) % len(modes)
			break
		}
	}
	nextMode := modes[nextIdx]
	if err := applyModelPermissionMode(m, nextMode); err != nil {
		m.messages = append(m.messages, Message{
			Role:      "error",
			Content:   "permission mode unchanged: " + err.Error(),
			Timestamp: now,
		})
	}
	// Mode change is shown in the footer ("· plan mode on (shift+tab to
	// cycle)"). We deliberately don't append a chat message — every
	// Shift+Tab press would otherwise pollute history.
}
