package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// applyScreenResult bridges Phase C interactive widgets to the live
// agent loop. Called from tui_update.go when an activeScreen reports
// Done(); type-asserts to each widget and applies any committed result
// before the parent clears the screen reference.
//
// Cancellation is screen-internal: each widget exposes a Result-style
// method that returns the empty string (or other zero value) when the
// user pressed Esc, so the apply step naturally no-ops.
//
// Returns a tea.Cmd when the apply step needs to schedule additional
// work (e.g. /help → "Enter on a command row" must dispatch that
// command which itself produces commands). Returns nil for fire-and-
// forget applies.
func (m *Model) applyScreenResult(s screen.Screen) tea.Cmd {
	switch w := s.(type) {
	case *screen.EffortScreen:
		applied := w.Applied()
		if applied == "" {
			return nil // user cancelled
		}
		// Map widget label → llm.Effort. "off" maps to EffortDefault
		// so the next request lets the provider decide.
		var e llm.Effort
		switch applied {
		case "off":
			e = llm.EffortDefault
		case "low":
			e = llm.EffortLow
		case "medium":
			e = llm.EffortMedium
		case "high":
			e = llm.EffortHigh
		default:
			return nil
		}
		m.loop.Effort = e
		effortState = applied
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   "effort: " + applied,
			Timestamp: time.Now(),
		})

	case *screen.ModelScreen:
		applied := w.Applied()
		if applied == "" {
			return nil
		}
		m.model = applied
		m.loop.Model = applied
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   "model: " + applied,
			Timestamp: time.Now(),
		})

	case *screen.ThemeScreen:
		applied := w.Applied()
		if applied == "" {
			return nil
		}
		if name := SwitchTheme(applied); name != "" {
			m.messages = append(m.messages, Message{
				Role:      "success",
				Content:   "theme: " + name,
				Timestamp: time.Now(),
			})
		}

	case *screen.PermissionsScreen:
		applied := w.Applied()
		if applied == "" {
			return nil
		}
		if applied == string(m.gate.Mode()) {
			return nil
		}
		m.gate.SetMode(permission.Mode(applied))
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   "permission mode: " + applied,
			Timestamp: time.Now(),
		})

	case *screen.HelpScreen:
		// User pressed Enter on a /help command row → dispatch that
		// command immediately. Mirrors claude-code's behavior where the
		// commands tab is also a launcher (image #14: cursor on
		// /advisor + Enter runs it).
		picked := w.Selected()
		if picked == "" {
			return nil // Esc / cancelled / cursor on non-selectable row
		}
		m.input.SetValue("/" + picked)
		_, cmd := m.handleSubmit()
		return cmd

	case *screen.PickerScreen:
		// PickerScreen serves multiple list-style commands; route on
		// the `command` field the picker was opened with.
		picked := w.Selected()
		if picked == "" {
			return nil
		}
		switch w.Command() {
		case "/sessions":
			// Resume the picked session — switch the active session ID
			// and reload its history.
			if m.session == nil {
				return nil
			}
			if hdr, msgs, err := m.session.Load(picked); err == nil && hdr != nil {
				m.sessionID = picked
				m.loop.Restore(msgs)
				m.messages = nil
				m.toolEvents = nil
				m.totalTokens.Reset()
				m.messages = append(m.messages, Message{
					Role:      "success",
					Content:   "resumed session: " + shortID(picked),
					Timestamp: time.Now(),
				})
			} else if err != nil {
				m.messages = append(m.messages, Message{
					Role:      "error",
					Content:   "resume failed: " + err.Error(),
					Timestamp: time.Now(),
				})
			}
		case "/skills":
			// Show the picked skill's full prompt body via Skill tool
			// hint — for now surface a confirmation pointing at /skill
			// install or /skill describe (TODO: detail screen).
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "skill: " + picked + " — use the Skill tool to invoke",
				Timestamp: time.Now(),
			})
		case "/tools":
			// Surface the tool name + a hint that tool dispatch is
			// LLM-driven (the user can't directly invoke a tool —
			// tools fire when the agent decides to). The picker still
			// works as a "browse + remind myself what's available" UX.
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "tool: " + picked + " — invoked by the agent on demand, not directly",
				Timestamp: time.Now(),
			})
		}
	}
	return nil
}
