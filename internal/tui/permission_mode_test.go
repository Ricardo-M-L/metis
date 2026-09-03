package tui

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestApplyModelPermissionModeAllowsActiveTurnAtToolBoundary(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	m := &Model{
		gate: permission.New(permission.ModeDefault),
		loop: &agent.Loop{},
		ext:  ExternalHooks{Sandbox: manager},
	}
	if err := applyModelPermissionMode(m, permission.ModeFullAccess); err != nil {
		t.Fatalf("enter fullAccess test posture: %v", err)
	}
	m.turnActive = true

	err = applyModelPermissionMode(m, permission.ModeDefault)
	if err != nil {
		t.Fatalf("active-turn permission transition: %v", err)
	}
	if got := m.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("active-turn transition mode = %q, want default", got)
	}
	if m.loop.IsPlanMode() {
		t.Fatal("active-turn transition unexpectedly enabled plan mode")
	}
	if got := m.loop.PrePlanMode(); got != "" {
		t.Fatalf("active-turn transition retained pre-plan lineage %q", got)
	}
	if manager.State().FullAccessRequired {
		t.Fatal("active-turn transition did not restore the process sandbox")
	}
}

func TestApplyPermissionModeDoesNotReuseStaleBypassPlanLineage(t *testing.T) {
	gate := permission.New(permission.ModeBypassPermissions)
	loop := &agent.Loop{}
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.RequireCredentialIsolation(true); err != nil {
		t.Fatal(err)
	}

	if err := applyPermissionMode(gate, loop, manager, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if got := loop.PrePlanMode(); got != string(permission.ModeBypassPermissions) {
		t.Fatalf("first plan snapshot = %q, want bypassPermissions", got)
	}
	if err := applyPermissionMode(gate, loop, manager, permission.ModeDefault); err != nil {
		t.Fatal(err)
	}
	if got := loop.PrePlanMode(); got != "" {
		t.Fatalf("leaving plan retained snapshot %q", got)
	}
	if err := applyPermissionMode(gate, loop, manager, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if got := loop.PrePlanMode(); got != string(permission.ModeDefault) {
		t.Fatalf("second plan snapshot = %q, want current default posture", got)
	}
}

func TestApplyModelBypassRejectsStalePermissionPrompt(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	reply := make(chan agent.PermissionDecision, 1)
	m := &Model{
		gate:         permission.New(permission.ModeDefault),
		loop:         &agent.Loop{},
		ext:          ExternalHooks{Sandbox: manager},
		permActive:   true,
		permReply:    reply,
		permQuestion: "allow stale tool call?",
		permTool:     "Bash",
		permArgs:     "example",
	}
	if err := applyModelPermissionMode(m, permission.ModeBypassPermissions); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-reply:
		if got != agent.PermissionDecisionDeny {
			t.Fatalf("stale prompt decision = %v, want deny", got)
		}
	default:
		t.Fatal("stale prompt was not resolved")
	}
	if m.permActive || m.permReply != nil || m.permQuestion != "" || m.permTool != "" || m.permArgs != "" {
		t.Fatal("stale prompt state was not cleared")
	}
}

func TestActiveTurnModeChangeRejectsOldPromptBeforeQueueingTransition(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	reply := make(chan agent.PermissionDecision, 1)
	m := &Model{
		gate:       permission.New(permission.ModeDefault),
		loop:       &agent.Loop{},
		ext:        ExternalHooks{Sandbox: manager},
		turnActive: true,
		permActive: true,
		permReply:  reply,
	}
	cmd := m.requestModelPermissionMode(permission.ModeBypassPermissions, "", "")
	if cmd == nil {
		t.Fatal("active-turn mode change did not queue a transition")
	}
	select {
	case got := <-reply:
		if got != agent.PermissionDecisionDeny {
			t.Fatalf("superseded prompt decision = %v, want deny", got)
		}
	default:
		t.Fatal("superseded prompt was not resolved before transition queued")
	}
	if m.permActive || m.permReply != nil {
		t.Fatal("superseded prompt UI state was not cleared")
	}

	updated, _ := m.Update(cmd())
	*m = *(updated.(*Model))
	if got := m.gate.Mode(); got != permission.ModeBypassPermissions {
		t.Fatalf("settled permission mode = %q, want bypassPermissions", got)
	}
}
