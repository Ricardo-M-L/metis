package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/tui/overlay"
)

func TestEscOverlayClosesBeforeCancellingTurn(t *testing.T) {
	for _, name := range []string{"palette", "search", "history", "atmention", "effort", "btw", "model", "taskpanel", "permission", "askuser", "askuser_freeform"} {
		t.Run(name, func(t *testing.T) {
			m := newSlashTestModel(t)
			var closed func() bool
			switch name {
			case "palette":
				// Exercise the actual /model keystream, not just a synthetic flag.
				m.turnActive = true
				for _, r := range "/model" {
					m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
				}
				if !m.showPalette {
					t.Fatal("typing /model did not open command completion")
				}
				closed = func() bool { return !m.showPalette }
			case "search":
				m.openTranscriptSearch()
				closed = func() bool { return !m.showSearch }
			case "history":
				m.showHistory = true
				closed = func() bool { return !m.showHistory }
			case "atmention":
				m.atActive = true
				m.atMatched = []string{"main.go"}
				closed = func() bool { return !m.atActive }
			case "effort":
				m.openInlineEffortPicker()
				closed = func() bool { return m.effortPicker == nil }
			case "btw":
				m.overlays = overlay.New()
				// Do not execute OnPush: no provider or background request is needed.
				m.overlays.Push(overlay.NewBtwOverlay(context.Background(), "local fixture", nil))
				closed = func() bool { return !m.overlays.Active() }
			case "model":
				configureModelWidgetAnthropic(m)
				m.input.SetValue("/model")
				pressEnter(t, m)
				if m.activeScreen == nil {
					t.Fatal("idle /model did not open the model picker")
				}
				closed = func() bool { return m.activeScreen == nil }
			case "taskpanel":
				m.showTaskPanel = true
				closed = func() bool { return !m.showTaskPanel }
			case "permission":
				m.permActive = true
				reply := make(chan agent.PermissionDecision, 1)
				m.permReply = reply
				closed = func() bool {
					select {
					case decision := <-reply:
						return decision == agent.PermissionDecisionDeny && !m.permActive && m.permReply == nil
					default:
						return false
					}
				}
			case "askuser", "askuser_freeform":
				m.askUserActive = true
				m.askUserFreeformOn = name == "askuser_freeform"
				reply := make(chan string, 1)
				m.askUserReply = reply
				closed = func() bool {
					select {
					case answer := <-reply:
						return answer == "" && !m.askUserActive && m.askUserReply == nil
					default:
						return false
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.turnActive = true
			m.turnCancel = cancel
			m.spinnerActive = true
			m.backgroundResumeAllowed = true
			m.queuedPrompts = []queuedItem{{Text: "next task"}}

			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if !closed() {
				t.Fatal("first Esc did not dismiss the focused panel safely")
			}
			if ctx.Err() != nil || m.turnCancelledByUser || m.turnCancel == nil || !m.turnActive || !m.spinnerActive {
				t.Fatal("closing the panel cancelled or changed the running task")
			}
			if !m.backgroundResumeAllowed || len(m.queuedPrompts) != 1 {
				t.Fatal("closing the panel changed the task's continuation or queue")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if ctx.Err() != context.Canceled || !m.turnCancelledByUser || m.turnCancel != nil || m.spinnerActive {
				t.Fatal("second Esc on the main screen did not cancel the task")
			}
			if len(m.queuedPrompts) != 0 || !m.turnActive {
				t.Fatal("task cancellation must drop queued work but still wait for Run to exit")
			}
		})
	}
}

func TestEscOverlayDoesNotCancelOAuthBehindPalette(t *testing.T) {
	m := newSlashTestModel(t)
	oauthCancelled, turnCancelled := 0, 0
	m.mcpLoginPending = true
	m.mcpLoginCancel = func() { oauthCancelled++ }
	m.turnActive = true
	m.turnCancel = func() { turnCancelled++ }
	m.showPalette = true
	m.matchCommands()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if oauthCancelled != 0 || turnCancelled != 0 || !m.mcpLoginPending || m.showPalette {
		t.Fatal("panel dismissal must not cancel either independent operation")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if oauthCancelled != 1 || turnCancelled != 1 {
		t.Fatalf("main-screen Esc should retain OAuth/turn cancellation: %d/%d", oauthCancelled, turnCancelled)
	}
}

func TestEscOverlayDuringCancellationKeepsNewQueue(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.turnCancelledByUser = true
	m.showPalette = true
	m.matchCommands()
	m.queuedPrompts = []queuedItem{{Text: "follow-up after cancellation"}}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.showPalette || len(m.queuedPrompts) != 1 || !m.turnActive {
		t.Fatal("closing a panel during cleanup dropped newly queued input")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.queuedPrompts) != 0 || !m.turnActive {
		t.Fatal("repeated Esc without a panel must clear the queue without faking Run completion")
	}
}

func TestEscOverlayLeavesCtrlCBehaviourUnchanged(t *testing.T) {
	m := newSlashTestModel(t)
	cancelled := false
	m.turnActive = true
	m.turnCancel = func() { cancelled = true }
	m.showPalette = true
	m.matchCommands()
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !cancelled || !m.turnCancelledByUser {
		t.Fatal("palette priority for Esc must not intercept Ctrl+C")
	}
}

func TestEscOverlayInvisibleAtMentionDoesNotConsumeCancellation(t *testing.T) {
	m := newSlashTestModel(t)
	cancelled := false
	m.turnActive = true
	m.turnCancel = func() { cancelled = true }
	m.atActive = true
	m.atMatched = nil // No dropdown is rendered without matches.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !cancelled {
		t.Fatal("invisible completion state swallowed task cancellation")
	}
}

func TestEscOverlayDismissesOnlyTopLayer(t *testing.T) {
	m := newSlashTestModel(t)
	cancelled := false
	m.turnActive = true
	m.turnCancel = func() { cancelled = true }
	m.showPalette = true
	m.matchCommands()
	m.overlays = overlay.New()
	m.overlays.Push(overlay.NewBtwOverlay(context.Background(), "top panel", nil))
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.overlays.Active() || !m.showPalette || cancelled {
		t.Fatal("first Esc must dismiss only the top overlay")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.showPalette || cancelled {
		t.Fatal("second Esc must dismiss the remaining panel without cancelling")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !cancelled {
		t.Fatal("Esc with all panels closed must still cancel the task")
	}
}
