package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// A successful task may have left a background process behind. Local history
// edits revoke that task's automatic continuation: the process can still
// report completion, but it cannot act on a cleared or rewound conversation.
func TestBackgroundCompletionDoesNotReviveReplacedHistory(t *testing.T) {
	for _, boundary := range []string{"clear", "undo", "rewind_conversation", "rewind_summary", "session_switch"} {
		t.Run(boundary, func(t *testing.T) {
			m, provider := newBackgroundTestModel(t)
			m.slash = slash.NewRegistry()
			slash.RegisterAll(m.slash, nil)
			history := rewindHistory()
			m.loop.Restore(history)
			if err := m.session.ReplaceHistoryAndMark(m.sessionID, history, &m.historyCursor); err != nil {
				t.Fatal(err)
			}
			m.handleAgentEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "end_turn"})
			if !m.backgroundResumeAllowed {
				t.Fatal("normal completed task did not arm background continuation")
			}
			release := backgroundTestJob(t, m.loop.Jobs)
			wait := m.waitForBackgroundJob()

			switch boundary {
			case "clear":
				if err := m.Reload(ReloadOpts{}); err != nil {
					t.Fatal(err)
				}
				if len(m.loop.History()) != 0 {
					t.Fatal("clear did not replace history")
				}
			case "undo":
				m.input.SetValue("/undo")
				m.handleSubmit()
				if m.loop.CountTurns() != 1 || m.input.Value() != "second prompt" {
					t.Fatal("undo did not remove the task and preserve its editable prompt")
				}
			case "rewind_conversation":
				picker := screen.NewRewindScreen([]screen.RewindEntry{{Turn: 2, Prompt: "second prompt"}})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m.applyScreenResult(picker)
				if m.loop.CountTurns() != 1 || m.input.Value() != "second prompt" {
					t.Fatal("conversation rewind did not commit")
				}
			case "rewind_summary":
				configureRewindSummary(t, m)
				cmd := m.applyScreenResult(rewindSummaryPicker())
				if cmd == nil || !m.rewindSummaryPending {
					t.Fatal("summary rewind did not start")
				}
				m.Update(cmd())
				if m.rewindSummaryPending || m.input.Value() != "second prompt" {
					t.Fatal("summary rewind did not finish")
				}
				var found bool
				for _, message := range m.loop.History() {
					for _, block := range message.Content {
						found = found || strings.Contains(block.Text, "ASYNC_SUMMARY")
					}
				}
				if !found {
					t.Fatal("summary rewind did not replace history")
				}
			case "session_switch":
				target := &session.Header{ID: "background-target", Model: "test-model", System: "target-system", Mode: string(permission.ModeDefault)}
				if err := m.session.WriteHeaderFull(*target); err != nil {
					t.Fatal(err)
				}
				if err := m.activateSession(target.ID, target, nil, true); err != nil {
					t.Fatal(err)
				}
				if m.sessionID != target.ID || len(m.loop.History()) != 0 {
					t.Fatal("session activation did not replace source history")
				}
			}
			if m.backgroundResumeAllowed {
				t.Fatal("history replacement retained authority to continue the old task")
			}
			release()
			m.Update(wait())
			if m.turnActive || len(provider.requests) != 0 {
				t.Fatal("late completion revived a discarded task")
			}
		})
	}
}
