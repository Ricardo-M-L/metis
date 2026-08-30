package runtime

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

type permissionPlanProbe struct {
	prePlan string
	plan    bool
}

func (p *permissionPlanProbe) PrePlanMode() string        { return p.prePlan }
func (p *permissionPlanProbe) SetPrePlanMode(mode string) { p.prePlan = mode }
func (p *permissionPlanProbe) SetPlanMode(plan bool)      { p.plan = plan }

func TestApplyPermissionModeKeepsBypassPlanIsolationAndRestoresSandbox(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	controller := &permissionPlanProbe{}

	if err := ApplyPermissionMode(gate, controller, manager, permission.ModeBypassPermissions); err != nil {
		t.Fatal(err)
	}
	if gate.Mode() != permission.ModeBypassPermissions || manager.EffectiveMode() != sandbox.ModePermissions {
		t.Fatalf("bypass transition = gate %s sandbox %s", gate.Mode(), manager.EffectiveMode())
	}

	if err := ApplyPermissionMode(gate, controller, manager, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if !controller.plan || controller.prePlan != string(permission.ModeBypassPermissions) || manager.EffectiveMode() != sandbox.ModePermissions {
		t.Fatalf("plan lineage = %+v sandbox %s", controller, manager.EffectiveMode())
	}

	if err := ApplyPermissionMode(gate, controller, manager, permission.ModeDefault); err != nil {
		t.Fatal(err)
	}
	if controller.plan || controller.prePlan != "" || manager.EffectiveMode() != sandbox.ModeOff {
		t.Fatalf("default transition = %+v sandbox %s", controller, manager.EffectiveMode())
	}
}

func TestApplyPermissionModeFailureDoesNotCommitBypass(t *testing.T) {
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeDefault)
	if err := ApplyPermissionMode(gate, &permissionPlanProbe{}, manager, permission.ModeBypassPermissions); err == nil {
		t.Fatal("expected closed sandbox manager error")
	}
	if got := gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("failed transition committed gate mode %s", got)
	}
}

func TestRestorePermissionModeStatePreservesPlanLineage(t *testing.T) {
	gate := permission.New(permission.ModePlan)
	controller := &permissionPlanProbe{prePlan: string(permission.ModeDefault), plan: true}
	state, err := CapturePermissionModeState(gate, controller)
	if err != nil {
		t.Fatal(err)
	}

	apply := func(mode permission.Mode) error {
		return ApplyPermissionMode(gate, controller, nil, mode)
	}
	if err := apply(permission.ModeAcceptEdits); err != nil {
		t.Fatal(err)
	}
	if err := RestorePermissionModeState(state, apply); err != nil {
		t.Fatal(err)
	}
	if got := gate.Mode(); got != permission.ModePlan {
		t.Fatalf("restored mode = %q, want plan", got)
	}
	if got := controller.PrePlanMode(); got != string(permission.ModeDefault) {
		t.Fatalf("restored pre-plan mode = %q, want default", got)
	}
	if !controller.plan {
		t.Fatal("restored controller is not in plan mode")
	}
}

func TestSynchronizeRestoredPermissionStateUsesCommittedGateMode(t *testing.T) {
	gate := permission.New(permission.ModeDefault)
	controller := &permissionPlanProbe{}
	gate.SetModeChangeListener(func(mode permission.Mode) {
		controller.SetPlanMode(mode == permission.ModePlan)
		if mode == permission.ModePlan {
			gate.SetMode(permission.ModeDontAsk)
		}
	})

	controller.SetPrePlanMode(string(permission.ModeBypassPermissions))
	gate.ResetSessionState(permission.ModePlan, nil)
	committed := SynchronizeRestoredPermissionState(gate, controller, string(permission.ModeBypassPermissions))

	if committed != permission.ModeDontAsk || gate.Mode() != permission.ModeDontAsk {
		t.Fatalf("committed mode = %q gate=%q, want dontAsk", committed, gate.Mode())
	}
	if controller.plan {
		t.Fatal("controller retained stale requested plan state")
	}
	if got := controller.PrePlanMode(); got != "" {
		t.Fatalf("controller retained stale pre-plan lineage %q", got)
	}
}
