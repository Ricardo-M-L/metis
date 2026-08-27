package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/themes"
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
	case *screen.ConfigScreen:
		return m.applyConfigScreen(w)
	case *screen.RewindScreen:
		return m.applyRewindScreen(w)

	case *screen.EffortScreen:
		applied := w.Applied()
		if applied == "" {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(effort dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
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
		m.loop.SetEffort(e)
		effortState = applied
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   "effort: " + applied,
			Timestamp: time.Now(),
		})

	case *screen.ModelScreen:
		providerPicker := w.Command() == "/provider"
		imageRecovery := m.imageRecoveryPending
		recoveryImageCount := m.imageRecoveryImageCount
		m.imageRecoveryPending = false
		m.imageRecoveryImageCount = 0
		choice, selected := w.AppliedChoice()
		if !selected || choice.ID == "" {
			content := "(model dialog dismissed)"
			if providerPicker {
				content = "(provider dialog dismissed)"
			} else if imageRecovery {
				content = "(vision model selection cancelled — prompt and attachments kept)"
			}
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   content,
				Timestamp: time.Now(),
			})
			return nil
		}
		// ModelScreen returns the provider profile together with the ID. This
		// is essential for dynamic [provider.custom.*] entries: carrying only
		// the model string would accidentally send it through the old backend.
		switchErr := m.switchModel(choice.ID, choice.Provider)
		if switchErr != nil {
			content := "model switch failed; previous model remains active: " + switchErr.Error()
			if providerPicker {
				content = "provider switch failed; previous provider remains active: " + switchErr.Error()
			} else if imageRecovery {
				content += " · prompt and attachments kept"
			}
			m.messages = append(m.messages, Message{
				Role:      "warning",
				Content:   content,
				Timestamp: time.Now(),
			})
		} else {
			content := "model: " + m.model + "  ·  provider: " + m.providerName
			if providerPicker {
				content = "provider: " + m.providerName + "  ·  model: " + m.model
			} else if imageRecovery {
				content += fmt.Sprintf(" · prompt and %d image(s) kept — press Enter to send", recoveryImageCount)
			}
			m.messages = append(m.messages, Message{
				Role:      "success",
				Content:   content,
				Timestamp: time.Now(),
			})
		}

	case *screen.ThemeScreen:
		applied := w.Applied()
		if applied == "" {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(theme dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
		}
		if name := themes.SwitchTheme(applied); name != "" {
			m.messages = append(m.messages, Message{
				Role:      "success",
				Content:   "theme: " + name,
				Timestamp: time.Now(),
			})
		}

	case *screen.PermissionsScreen:
		applied := w.Applied()
		if applied == "" {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(permissions dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
		}
		if applied == string(m.gate.Mode()) {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(permissions dialog dismissed — no change)",
				Timestamp: time.Now(),
			})
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
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(help dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
		}
		m.input.SetValue("/" + picked)
		_, cmd := m.handleSubmit()
		return cmd

	case *screen.DetailScreen:
		// Esc on a detail screen — if the detail was opened from a
		// picker, re-dispatch the parent so the user lands back on the
		// list rather than chat. Otherwise no-op (Esc → chat).
		// Mirrors claude-code's stack semantics ("back to list").
		if parent := w.ParentCommand(); parent != "" {
			m.input.SetValue("/" + parent)
			_, cmd := m.handleSubmit()
			return cmd
		}

	case *screen.BodyScreen:
		// Body screens (/diff, /cost, /context, /version, /doctor, …)
		// are pure-display modals — no committed result. The trace
		// just confirms the user closed it. claude-code shows
		// "Diff dialog dismissed" / "Cost dialog dismissed" etc.
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "(" + w.Command() + " dialog dismissed)",
			Timestamp: time.Now(),
		})

	case *screen.PickerScreen:
		// PickerScreen serves multiple list-style commands; route on
		// the `command` field the picker was opened with.
		picked := w.Selected()
		if picked == "" {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(" + w.Command() + " dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
		}
		switch w.Command() {
		case "/sessions":
			// Resume the picked session — switch the active session ID
			// and reload its history.
			if m.session == nil {
				return nil
			}
			if err := m.session.CheckResumeSize(picked); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: err.Error(), Timestamp: time.Now()})
				return nil
			}
			if err := m.persistActiveSessionState(); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "resume failed: " + err.Error(), Timestamp: time.Now()})
				return nil
			}
			if hdr, msgs, err := m.session.Load(picked); err == nil && hdr != nil {
				workDirWarning := m.sessionWorkDirWarning(hdr)
				if err := m.activateSession(picked, hdr, msgs, true); err != nil {
					m.messages = append(m.messages, Message{Role: "warning", Content: m.sessionActivationWarning("resume", err), Timestamp: time.Now()})
					return nil
				}
				m.messages = append(m.messages, Message{
					Role:      "success",
					Content:   "resumed session: " + shortID(picked),
					Timestamp: time.Now(),
				})
				if workDirWarning != "" {
					m.messages = append(m.messages, Message{Role: "warning", Content: workDirWarning, Timestamp: time.Now()})
				}
			} else if err != nil {
				m.messages = append(m.messages, Message{
					Role:      "error",
					Content:   "resume failed: " + err.Error(),
					Timestamp: time.Now(),
				})
			}
		case "/skills":
			// Open DetailScreen with full prompt body, allowed tools,
			// when_to_use, and version metadata.
			if ds := m.skillDetailScreen(picked); ds != nil {
				ds.Resize(m.width, m.height)
				m.activeScreen = ds
			}
		case "/tools":
			// Open DetailScreen with description + JSON schema so the
			// user can see what arguments the tool takes.
			if ds := m.toolDetailScreen(picked); ds != nil {
				ds.Resize(m.width, m.height)
				m.activeScreen = ds
			}
		}

	// === Feature 1: Resume/Fork Picker ===
	case *screen.ResumeScreen:
		action := w.Action()
		sid := w.Selected()
		if sid == "" && action != screen.ResumeActionFresh {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(resume dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
		}
		if action == screen.ResumeActionFresh {
			if m.session == nil || m.loop == nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "cannot start a fresh session: session runtime unavailable", Timestamp: time.Now()})
				return nil
			}
			if err := m.persistActiveSessionState(); err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "fresh session failed: " + err.Error(), Timestamp: time.Now()})
				return nil
			}
			newID, hdr, err := m.createFreshSession()
			if err != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "fresh session failed: " + err.Error(), Timestamp: time.Now()})
				return nil
			}
			if err := m.activateCreatedSession(newID, hdr, nil); err != nil {
				m.messages = append(m.messages, Message{Role: "warning", Content: m.sessionActivationWarning("fresh session activation", err), Timestamp: time.Now()})
				return nil
			}
			m.messages = append(m.messages, Message{
				Role:      "success",
				Content:   "started fresh session: " + shortID(newID),
				Timestamp: time.Now(),
			})
			return nil
		}
		if m.session == nil {
			return nil
		}
		if err := m.session.CheckResumeSize(sid); err != nil {
			m.messages = append(m.messages, Message{Role: "error", Content: err.Error(), Timestamp: time.Now()})
			return nil
		}
		if err := m.persistActiveSessionState(); err != nil {
			m.messages = append(m.messages, Message{Role: "error", Content: "resume failed: " + err.Error(), Timestamp: time.Now()})
			return nil
		}
		hdr, msgs, err := m.session.Load(sid)
		if err != nil || hdr == nil {
			detail := "session header missing"
			if err != nil {
				detail = err.Error()
			}
			m.messages = append(m.messages, Message{
				Role:      "error",
				Content:   "resume failed: " + detail,
				Timestamp: time.Now(),
			})
			return nil
		}
		if action == screen.ResumeActionFork {
			newID, newHdr, branchErr := m.forkSession(sid, msgs)
			if branchErr != nil {
				m.messages = append(m.messages, Message{Role: "error", Content: "fork failed: " + branchErr.Error(), Timestamp: time.Now()})
				return nil
			}
			sid = newID
			hdr = newHdr
		}
		workDirWarning := ""
		if action == screen.ResumeActionResume {
			workDirWarning = m.sessionWorkDirWarning(hdr)
		}
		label := "resumed"
		failureAction := "resume"
		if action == screen.ResumeActionFork {
			label = "forked"
			failureAction = "fork activation"
		}
		var activationErr error
		if action == screen.ResumeActionFork {
			activationErr = m.activateCreatedSession(sid, hdr, msgs)
		} else {
			activationErr = m.activateSession(sid, hdr, msgs, true)
		}
		if activationErr != nil {
			m.messages = append(m.messages, Message{Role: "warning", Content: m.sessionActivationWarning(failureAction, activationErr), Timestamp: time.Now()})
			return nil
		}
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   label + " session: " + shortID(sid),
			Timestamp: time.Now(),
		})
		if workDirWarning != "" {
			m.messages = append(m.messages, Message{Role: "warning", Content: workDirWarning, Timestamp: time.Now()})
		}

	// === Feature 3: Diff Viewer ===
	case *screen.DiffViewerScreen:
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "(diff viewer dismissed)",
			Timestamp: time.Now(),
		})

	// === Feature 4: Multi-Agent Visualization ===
	case *screen.MultiAgentScreen:
		picked := w.Selected()
		if picked == "" {
			m.messages = append(m.messages, Message{
				Role:      "info",
				Content:   "(agents dialog dismissed)",
				Timestamp: time.Now(),
			})
			return nil
		}
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "selected agent: " + picked,
			Timestamp: time.Now(),
		})
	}
	return nil
}
