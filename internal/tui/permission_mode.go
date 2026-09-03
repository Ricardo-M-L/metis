package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// applyPermissionMode is the single external/user-driven mode transition for
// the TUI. Entering plan snapshots the current posture; leaving plan clears
// the snapshot. This prevents bypass -> plan -> default -> plan from reusing a
// stale bypass lineage and silently disabling AskUser or auto-approving Exit.
func applyPermissionMode(gate *permission.Gate, loop *agent.Loop, manager *sandbox.Manager, mode permission.Mode) error {
	return rtpkg.ApplyPermissionMode(gate, loop, manager, mode)
}

func applyModelPermissionMode(m *Model, mode permission.Mode) error {
	if m == nil {
		return nil
	}
	if err := applyPermissionMode(m.gate, m.loop, m.ext.Sandbox, mode); err != nil {
		return err
	}
	// A permission request belongs to the posture under which it was issued.
	// Entering bypass while an older Ask is visible must not leave the agent
	// blocked on UI that the new mode promises never to show. Reject the stale
	// request (rather than silently granting it) and let the loop continue under
	// the newly committed posture.
	if (mode == permission.ModeBypassPermissions || mode == permission.ModeFullAccess) && m.permActive {
		m.executePermission("n")
	}
	return nil
}

type permissionModeTransitionResultMsg struct {
	seq            uint64
	mode           permission.Mode
	err            error
	successRole    string
	successContent string
}

// requestModelPermissionMode applies idle transitions synchronously and queues
// active-turn transitions in tea.Cmd. The runtime permission coordinator holds
// the write side of Gate's dispatch barrier, so a tool that already entered
// Execute completes with its original posture while every later batch waits
// for the new Gate+Sandbox state to settle.
func (m *Model) requestModelPermissionMode(mode permission.Mode, successRole, successContent string) tea.Cmd {
	if m == nil {
		return nil
	}
	mode = permission.CanonicalMode(string(mode))
	if !m.turnActive {
		if err := applyModelPermissionMode(m, mode); err != nil {
			m.messages = append(m.messages, Message{Role: "error", Content: "permission mode unchanged: " + err.Error(), Timestamp: time.Now()})
		} else if successContent != "" {
			m.messages = append(m.messages, Message{Role: successRole, Content: successContent, Timestamp: time.Now()})
		}
		return nil
	}
	if m.permissionModePending {
		return nil
	}

	// A visible Ask belongs to the old posture and keeps its dispatch lease
	// until answered. Deny it before requesting the transition so the writer
	// cannot deadlock behind a prompt that the operator just superseded.
	if m.permActive {
		m.executePermission("n")
	}
	m.permissionModeSeq++
	seq := m.permissionModeSeq
	m.permissionModePending = true
	m.permissionModeTarget = mode
	gate, loop, manager := m.gate, m.loop, m.ext.Sandbox
	return func() tea.Msg {
		err := applyPermissionMode(gate, loop, manager, mode)
		return permissionModeTransitionResultMsg{
			seq: seq, mode: mode, err: err,
			successRole: successRole, successContent: successContent,
		}
	}
}

func (m *Model) handlePermissionModeTransitionResult(result permissionModeTransitionResultMsg) {
	if m == nil || result.seq != m.permissionModeSeq {
		return
	}
	m.permissionModePending = false
	m.permissionModeTarget = ""
	if result.err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: "permission mode unchanged: " + result.err.Error(), Timestamp: time.Now()})
		return
	}
	if m.gate != nil {
		committed := m.gate.Mode()
		if committed == result.mode {
			if result.successContent != "" {
				m.messages = append(m.messages, Message{Role: result.successRole, Content: result.successContent, Timestamp: time.Now()})
			}
			return
		}
		m.messages = append(m.messages, Message{
			Role: "info",
			Content: "permission mode change to " + string(result.mode) +
				" was superseded by " + string(committed),
			Timestamp: time.Now(),
		})
		return
	}
	if result.successContent != "" {
		m.messages = append(m.messages, Message{Role: result.successRole, Content: result.successContent, Timestamp: time.Now()})
	}
}

func applyREPLPermissionMode(r *REPL, mode permission.Mode) error {
	if r == nil {
		return nil
	}
	return applyPermissionMode(r.Gate, r.Loop, r.sandbox, mode)
}
