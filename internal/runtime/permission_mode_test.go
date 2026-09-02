package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

type permissionPlanProbe struct {
	prePlan string
	plan    bool
}

type blockingPermissionPlanProbe struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type permissionSnapshotProbe struct {
	mu      sync.Mutex
	prePlan string
	plan    bool
	block   bool
	value   string
	entered chan struct{}
	release chan struct{}
}

func (p *permissionSnapshotProbe) PrePlanMode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prePlan
}

func (p *permissionSnapshotProbe) SetPrePlanMode(mode string) {
	p.mu.Lock()
	p.prePlan = mode
	block := p.block && mode == p.value
	if block {
		p.block = false
	}
	entered, release := p.entered, p.release
	p.mu.Unlock()
	if block {
		close(entered)
		<-release
	}
}

func (p *permissionSnapshotProbe) SetPlanMode(plan bool) {
	p.mu.Lock()
	p.plan = plan
	p.mu.Unlock()
}

func (p *permissionSnapshotProbe) blockNextPrePlanWrite(value string) (<-chan struct{}, chan<- struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.block = true
	p.value = value
	p.entered = make(chan struct{})
	p.release = make(chan struct{})
	return p.entered, p.release
}

func (*blockingPermissionPlanProbe) PrePlanMode() string   { return "" }
func (*blockingPermissionPlanProbe) SetPrePlanMode(string) {}
func (p *blockingPermissionPlanProbe) SetPlanMode(bool) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
}

func (p *permissionPlanProbe) PrePlanMode() string        { return p.prePlan }
func (p *permissionPlanProbe) SetPrePlanMode(mode string) { p.prePlan = mode }
func (p *permissionPlanProbe) SetPlanMode(plan bool)      { p.plan = plan }

func TestValidateRestoredPermissionStateFromSourceFiltersUntrustedFullAccess(t *testing.T) {
	tests := []struct {
		name        string
		rawMode     string
		rawPrePlan  string
		fallback    permission.Mode
		trusted     bool
		wantMode    permission.Mode
		wantPrePlan string
		wantStored  bool
	}{
		{
			name:    "untrusted direct full access falls back",
			rawMode: string(permission.ModeFullAccess), fallback: permission.ModeDefault,
			wantMode: permission.ModeDefault,
		},
		{
			name:    "untrusted plan keeps plan but drops full access lineage",
			rawMode: string(permission.ModePlan), rawPrePlan: string(permission.ModeFullAccess),
			fallback: permission.ModeAcceptEdits, wantMode: permission.ModePlan,
			wantPrePlan: string(permission.ModeAcceptEdits), wantStored: true,
		},
		{
			name:    "untrusted ordinary stored mode remains usable",
			rawMode: string(permission.ModeDontAsk), fallback: permission.ModeDefault,
			wantMode: permission.ModeDontAsk, wantStored: true,
		},
		{
			name:    "trusted full access remains usable",
			rawMode: string(permission.ModeFullAccess), fallback: permission.ModeDefault, trusted: true,
			wantMode: permission.ModeFullAccess, wantStored: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, prePlan, stored, err := ValidateRestoredPermissionStateFromSource(
				tt.rawMode, tt.rawPrePlan, tt.fallback, tt.trusted,
			)
			if err != nil {
				t.Fatal(err)
			}
			if mode != tt.wantMode || prePlan != tt.wantPrePlan || stored != tt.wantStored {
				t.Fatalf("validated = (%q, %q, %v), want (%q, %q, %v)",
					mode, prePlan, stored, tt.wantMode, tt.wantPrePlan, tt.wantStored)
			}
		})
	}
}

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

func TestApplyPermissionModeFullAccessDisablesSandboxAndRestoresConfiguredMode(t *testing.T) {
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "permissions", Network: sandbox.NetworkBlock, TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	controller := &permissionPlanProbe{}

	if err := ApplyPermissionMode(gate, controller, manager, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	state := manager.State()
	if gate.Mode() != permission.ModeFullAccess || !state.FullAccessRequired || state.CredentialIsolationRequired || state.Effective != sandbox.ModeOff || !state.AutoAllow {
		t.Fatalf("fullAccess transition = gate %s sandbox %+v", gate.Mode(), state)
	}
	if manager.NetworkPolicy() != sandbox.NetworkAllow {
		t.Fatalf("fullAccess network = %q, want allow", manager.NetworkPolicy())
	}

	if err := ApplyPermissionMode(gate, controller, manager, permission.ModeDefault); err != nil {
		t.Fatal(err)
	}
	state = manager.State()
	if gate.Mode() != permission.ModeDefault || state.FullAccessRequired || state.Effective != sandbox.ModePermissions || manager.NetworkPolicy() != sandbox.NetworkBlock {
		t.Fatalf("restored transition = gate %s sandbox %+v network=%s", gate.Mode(), state, manager.NetworkPolicy())
	}
}

func TestApplyPermissionModeFullAccessCommitsGateBeforeDisablingSandbox(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	observed := false
	gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode != permission.ModeFullAccess {
			return
		}
		observed = true
		if manager.State().FullAccessRequired {
			t.Fatal("sandbox was disabled before the Gate committed fullAccess")
		}
	})

	if err := ApplyPermissionMode(gate, &permissionPlanProbe{}, manager, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	if !observed || !manager.State().FullAccessRequired {
		t.Fatalf("transition observation=%v state=%+v", observed, manager.State())
	}
}

func TestApplyPermissionModeSerializesConcurrentFullAccessAndDefault(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	enteredFull := make(chan struct{})
	releaseFull := make(chan struct{})
	var once sync.Once
	gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode == permission.ModeFullAccess {
			once.Do(func() { close(enteredFull) })
			<-releaseFull
		}
		if err := ReconcilePermissionSandbox(manager, nil, mode); err != nil {
			t.Errorf("listener reconcile %s: %v", mode, err)
		}
	})

	fullErr := make(chan error, 1)
	go func() {
		fullErr <- ApplyPermissionMode(gate, nil, manager, permission.ModeFullAccess)
	}()
	select {
	case <-enteredFull:
	case <-time.After(2 * time.Second):
		t.Fatal("fullAccess listener did not start")
	}

	defaultErr := make(chan error, 1)
	go func() {
		defaultErr <- ApplyPermissionMode(gate, nil, manager, permission.ModeDefault)
	}()
	select {
	case err := <-defaultErr:
		t.Fatalf("newer Apply returned before the older transition settled: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFull)
	if err := <-fullErr; err != nil {
		t.Fatalf("fullAccess Apply: %v", err)
	}
	if err := <-defaultErr; err != nil {
		t.Fatalf("default Apply: %v", err)
	}
	if state := manager.State(); gate.Mode() != permission.ModeDefault || state.FullAccessRequired || state.CredentialIsolationRequired {
		t.Fatalf("final posture = gate %s sandbox %+v, want default sandboxed", gate.Mode(), state)
	}
}

func TestApplyPermissionModeSerializesConcurrentFullAccessAndBypass(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	enteredFull := make(chan struct{})
	releaseFull := make(chan struct{})
	var once sync.Once
	gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode == permission.ModeFullAccess {
			once.Do(func() { close(enteredFull) })
			<-releaseFull
		}
		if err := ReconcilePermissionSandbox(manager, nil, mode); err != nil {
			t.Errorf("listener reconcile %s: %v", mode, err)
		}
	})

	fullErr := make(chan error, 1)
	go func() {
		fullErr <- ApplyPermissionMode(gate, nil, manager, permission.ModeFullAccess)
	}()
	select {
	case <-enteredFull:
	case <-time.After(2 * time.Second):
		t.Fatal("fullAccess listener did not start")
	}

	bypassErr := make(chan error, 1)
	go func() {
		bypassErr <- ApplyPermissionMode(gate, nil, manager, permission.ModeBypassPermissions)
	}()
	select {
	case err := <-bypassErr:
		t.Fatalf("newer bypass Apply returned before the older transition settled: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFull)
	if err := <-fullErr; err != nil {
		t.Fatalf("fullAccess Apply: %v", err)
	}
	if err := <-bypassErr; err != nil {
		t.Fatalf("bypass Apply: %v", err)
	}
	state := manager.State()
	if gate.Mode() != permission.ModeBypassPermissions || !state.CredentialIsolationRequired || state.FullAccessRequired {
		t.Fatalf("final posture = gate %s sandbox %+v, want isolated bypass", gate.Mode(), state)
	}
}

func TestApplyPermissionModeDetectsDirectSetModeSupersession(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	enteredFull := make(chan struct{})
	releaseFull := make(chan struct{})
	var once sync.Once
	gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode == permission.ModeFullAccess {
			once.Do(func() { close(enteredFull) })
			<-releaseFull
		}
		if err := ReconcilePermissionSandbox(manager, nil, mode); err != nil {
			t.Errorf("listener reconcile %s: %v", mode, err)
		}
	})

	applyErr := make(chan error, 1)
	go func() {
		applyErr <- ApplyPermissionMode(gate, nil, manager, permission.ModeFullAccess)
	}()
	select {
	case <-enteredFull:
	case <-time.After(2 * time.Second):
		t.Fatal("fullAccess listener did not start")
	}

	directDone := make(chan struct{})
	go func() {
		gate.SetMode(permission.ModeDefault)
		close(directDone)
	}()
	select {
	case <-directDone:
	case <-time.After(2 * time.Second):
		t.Fatal("direct newer SetMode blocked behind the older listener")
	}
	if decision, source := gate.Check(context.Background(), "Bash", "echo ok"); decision != permission.DecisionDeny || source != "mode:transition" {
		t.Fatalf("check during superseded transition = %v (%s), want deny mode:transition", decision, source)
	}

	close(releaseFull)
	err = <-applyErr
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("fullAccess Apply error = %v, want superseded conflict", err)
	}
	if state := manager.State(); gate.Mode() != permission.ModeDefault || state.FullAccessRequired || state.CredentialIsolationRequired {
		t.Fatalf("superseded posture = gate %s sandbox %+v, want default sandboxed", gate.Mode(), state)
	}
}

func TestApplyPermissionModeNoListenerDeniesUntilFallbackPostureSettles(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.SetPermissionPosture(false, true); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeFullAccess)
	controller := &blockingPermissionPlanProbe{entered: make(chan struct{}), release: make(chan struct{})}

	done := make(chan error, 1)
	go func() {
		done <- ApplyPermissionMode(gate, controller, manager, permission.ModeDefault)
	}()
	select {
	case <-controller.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not reach fallback posture window")
	}
	if !manager.State().FullAccessRequired {
		t.Fatal("test did not stop before fallback sandbox posture write")
	}
	if decision, source := gate.Check(context.Background(), "Bash", "echo ok"); decision != permission.DecisionDeny || source != "mode:transition" {
		t.Fatalf("check before fallback posture = %v (%s), want deny mode:transition", decision, source)
	}
	close(controller.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state := manager.State(); state.FullAccessRequired {
		t.Fatalf("fallback posture did not restore sandbox: %+v", state)
	}
}

func TestApplyPermissionModeFullAccessPreflightFailureDoesNotChangeGate(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeDefault)
	if err := ApplyPermissionMode(gate, &permissionPlanProbe{}, manager, permission.ModeFullAccess); err == nil {
		t.Fatal("closed sandbox manager unexpectedly accepted fullAccess")
	}
	if gate.Mode() != permission.ModeDefault {
		t.Fatalf("failed fullAccess preflight changed gate to %q", gate.Mode())
	}
}

func TestPreflightRestoredFullAccessRejectsClosedManager(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PreflightRestoredPermissionState(manager, permission.ModeFullAccess, ""); err == nil {
		t.Fatal("closed sandbox manager unexpectedly passed restored fullAccess preflight")
	}
}

func TestApplyPermissionModeFullAccessPlanLineageRestoresFullAccessOnExit(t *testing.T) {
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	controller := &permissionPlanProbe{}

	for _, mode := range []permission.Mode{permission.ModeFullAccess, permission.ModePlan} {
		if err := ApplyPermissionMode(gate, controller, manager, mode); err != nil {
			t.Fatal(err)
		}
	}
	if !controller.plan || controller.prePlan != string(permission.ModeFullAccess) || manager.State().FullAccessRequired {
		t.Fatalf("plan from fullAccess = controller %+v sandbox %+v", controller, manager.State())
	}
	if err := ApplyPermissionMode(gate, controller, manager, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	if controller.plan || controller.prePlan != "" || !manager.State().FullAccessRequired {
		t.Fatalf("exit plan to fullAccess = controller %+v sandbox %+v", controller, manager.State())
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

func TestCapturePermissionModeStateWaitsForFullAccessToPlanTransition(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	controller := &permissionSnapshotProbe{}
	if err := ApplyPermissionMode(gate, controller, manager, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}

	entered, release := controller.blockNextPrePlanWrite(string(permission.ModeFullAccess))
	transitionDone := make(chan error, 1)
	go func() {
		transitionDone <- ApplyPermissionMode(gate, controller, manager, permission.ModePlan)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fullAccess-to-plan transition did not reach its lineage write")
	}
	time.AfterFunc(100*time.Millisecond, func() { close(release) })

	state, captureErr := CapturePermissionModeState(gate, controller)
	if transitionErr := <-transitionDone; transitionErr != nil {
		t.Fatal(transitionErr)
	}
	if captureErr != nil {
		t.Fatalf("capture observed a torn fullAccess/fullAccess state: %v", captureErr)
	}
	if state.Mode != permission.ModePlan || state.PrePlanMode != string(permission.ModeFullAccess) {
		t.Fatalf("captured transition state = %+v, want plan with fullAccess lineage", state)
	}
}

func TestCapturePermissionModeStateDoesNotLosePlanFullAccessLineage(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	controller := &permissionSnapshotProbe{}
	for _, mode := range []permission.Mode{permission.ModeFullAccess, permission.ModePlan} {
		if err := ApplyPermissionMode(gate, controller, manager, mode); err != nil {
			t.Fatal(err)
		}
	}

	entered, release := controller.blockNextPrePlanWrite("")
	transitionDone := make(chan error, 1)
	go func() {
		transitionDone <- ApplyPermissionMode(gate, controller, manager, permission.ModeFullAccess)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("plan-to-fullAccess transition did not reach its lineage clear")
	}
	time.AfterFunc(100*time.Millisecond, func() { close(release) })

	state, captureErr := CapturePermissionModeState(gate, controller)
	if transitionErr := <-transitionDone; transitionErr != nil {
		t.Fatal(transitionErr)
	}
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	if state.Mode != permission.ModeFullAccess || state.PrePlanMode != "" {
		t.Fatalf("captured transition state = %+v, want committed fullAccess without stale lineage", state)
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
