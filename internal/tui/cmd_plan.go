package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	runtimepkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// activatePlanMode keeps the permission gate and loop controller in sync even
// in lightweight embedders that do not install the production Gate listener.
// It returns whether the session was already planning before this command.
func activatePlanMode(gate *permission.Gate, loop *agent.Loop, manager *sandbox.Manager) (bool, error) {
	already := false
	if gate != nil && gate.Mode() == permission.ModePlan {
		already = true
	}
	if loop != nil && loop.IsPlanMode() {
		already = true
	}
	if already {
		return true, nil
	}
	if err := applyPermissionMode(gate, loop, manager, permission.ModePlan); err != nil {
		return false, err
	}
	return false, nil
}

func updateCurrentPlanDraft(sessionID, description string) error {
	description = strings.TrimSpace(description)
	if description == "" {
		return nil
	}
	current, err := runtimepkg.ReadCurrentPlan(sessionID)
	if err != nil {
		return err
	}
	if current == "" || current == "# Current Plan" {
		return runtimepkg.WriteCurrentPlan(sessionID, "# Current Plan\n\n"+description)
	}
	return runtimepkg.WriteCurrentPlan(sessionID, current+"\n\n## Requested update\n\n"+description)
}

func currentPlanText(sessionID string) (string, error) {
	body, err := runtimepkg.ReadCurrentPlan(sessionID)
	if err != nil {
		return "", err
	}
	path := runtimepkg.CurrentPlanPath(sessionID)
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == "# Current Plan" {
		return "Current Plan\n\n(no plan drafted yet — use `/plan <description>` or `/plan open`)\n\nfile: " + path, nil
	}
	return "Current Plan\n\n" + body + "\n\nfile: " + path, nil
}

func planModeStartedText() string {
	return "(mode: plan — read-only exploration; use `/plan <description>` to begin or `/plan open` to edit)"
}

func (r *REPL) editCurrentPlan() error {
	path, err := runtimepkg.EnsureCurrentPlan(r.SessionID)
	if err != nil {
		return err
	}
	cmd := exec.Command(pickEditor(), path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "(plan saved: "+path+")")
	return nil
}

// persistPlanProposal recognizes ExitPlanMode's typed EventInfo envelope and
// makes the model's latest full proposal the same draft `/plan` displays and
// `/plan open` edits. The caller must surface a returned write error: silently
// accepting it would leave `/plan` showing a stale draft while the transcript
// displays the newer proposal.
func persistPlanProposal(sessionID, info string) (bool, error) {
	const prefix = "[plan proposal]"
	if !strings.HasPrefix(info, prefix) {
		return false, nil
	}
	body := strings.TrimSpace(strings.TrimPrefix(info, prefix))
	if body != "" {
		if err := runtimepkg.WriteCurrentPlan(sessionID, body); err != nil {
			return true, err
		}
	}
	return true, nil
}
