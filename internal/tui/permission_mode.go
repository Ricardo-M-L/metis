package tui

import (
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
	if mode == permission.ModeBypassPermissions && m.permActive {
		m.executePermission("n")
	}
	return nil
}

func applyREPLPermissionMode(r *REPL, mode permission.Mode) error {
	if r == nil {
		return nil
	}
	return applyPermissionMode(r.Gate, r.Loop, r.sandbox, mode)
}
