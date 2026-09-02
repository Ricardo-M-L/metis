package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestApplyModelPermissionModeRejectsActiveTurnWithoutChangingPosture(t *testing.T) {
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

	beforeMode := m.gate.Mode()
	beforePlan := m.loop.IsPlanMode()
	beforePrePlan := m.loop.PrePlanMode()
	beforeSandbox := manager.State()
	err = applyModelPermissionMode(m, permission.ModeDefault)
	if err == nil || !strings.Contains(err.Error(), "running turn is active") {
		t.Fatalf("active-turn permission transition error = %v, want explicit running-turn refusal", err)
	}
	if got := m.gate.Mode(); got != beforeMode {
		t.Fatalf("active-turn refusal changed gate mode: got %q, want %q", got, beforeMode)
	}
	if got := m.loop.IsPlanMode(); got != beforePlan {
		t.Fatalf("active-turn refusal changed plan state: got %v, want %v", got, beforePlan)
	}
	if got := m.loop.PrePlanMode(); got != beforePrePlan {
		t.Fatalf("active-turn refusal changed pre-plan lineage: got %q, want %q", got, beforePrePlan)
	}
	if got := manager.State(); got != beforeSandbox {
		t.Fatalf("active-turn refusal changed sandbox posture: got %+v, want %+v", got, beforeSandbox)
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
