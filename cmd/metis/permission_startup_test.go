package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestSetupRuntimeFreshPermissionStateIsCoherent(t *testing.T) {
	tests := []struct {
		mode        permission.Mode
		wantPlan    bool
		wantPrePlan string
	}{
		{mode: permission.ModeDefault},
		{mode: permission.ModeDontAsk},
		{mode: permission.ModeBypassPermissions},
		{mode: permission.ModePlan, wantPlan: true, wantPrePlan: string(permission.ModeDefault)},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			if tt.mode == permission.ModeBypassPermissions && !sandbox.Available() {
				t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
			}
			isolateResumeRuntimeTest(t)
			rt, err := setupRuntime(context.Background(), &cliFlags{
				mode: string(tt.mode), bare: true, noAuthWizard: true,
			})
			if err != nil {
				t.Fatalf("setupRuntime(%s): %v", tt.mode, err)
			}
			defer rt.Cleanup()

			if got := rt.gate.Mode(); got != tt.mode {
				t.Fatalf("gate mode = %q, want %q", got, tt.mode)
			}
			if got := rt.loop.IsPlanMode(); got != tt.wantPlan {
				t.Fatalf("plan controller = %v, want %v", got, tt.wantPlan)
			}
			if got := rt.loop.PrePlanMode(); got != tt.wantPrePlan {
				t.Fatalf("pre-plan mode = %q, want %q", got, tt.wantPrePlan)
			}
		})
	}
}

func TestSetupRuntimeResumePermissionStateHonorsExplicitModePriority(t *testing.T) {
	tests := []struct {
		name           string
		storedMode     permission.Mode
		storedPrePlan  string
		explicitMode   permission.Mode
		wantMode       permission.Mode
		wantPrePlan    string
		wantPlanActive bool
	}{
		{
			name:       "stored plan lineage survives without override",
			storedMode: permission.ModePlan, storedPrePlan: string(permission.ModeDontAsk),
			wantMode: permission.ModePlan, wantPrePlan: string(permission.ModeDontAsk), wantPlanActive: true,
		},
		{
			name:       "explicit non-plan clears stored plan lineage",
			storedMode: permission.ModePlan, storedPrePlan: string(permission.ModeDontAsk),
			explicitMode: permission.ModeDefault, wantMode: permission.ModeDefault,
		},
		{
			name:         "explicit plan gets safe default lineage",
			storedMode:   permission.ModeDontAsk,
			explicitMode: permission.ModePlan, wantMode: permission.ModePlan,
			wantPrePlan: string(permission.ModeDefault), wantPlanActive: true,
		},
		{
			name:       "stored dontAsk remains non-plan",
			storedMode: permission.ModeDontAsk, wantMode: permission.ModeDontAsk,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateResumeRuntimeTest(t)
			store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			id := "permission-priority-" + string(rune('a'+i))
			if err := store.WriteHeaderFull(session.Header{
				ID: id, Provider: "openai", Model: "stored-model",
				Mode: string(tt.storedMode), PrePlanMode: tt.storedPrePlan,
			}); err != nil {
				t.Fatal(err)
			}

			flags := &cliFlags{resumeID: id, bare: true, noAuthWizard: true}
			if tt.explicitMode != "" {
				flags.mode = string(tt.explicitMode)
			}
			rt, err := setupRuntime(context.Background(), flags)
			if err != nil {
				t.Fatalf("setupRuntime: %v", err)
			}
			defer rt.Cleanup()

			if got := rt.gate.Mode(); got != tt.wantMode {
				t.Fatalf("gate mode = %q, want %q", got, tt.wantMode)
			}
			if got := rt.loop.IsPlanMode(); got != tt.wantPlanActive {
				t.Fatalf("plan controller = %v, want %v", got, tt.wantPlanActive)
			}
			if got := rt.loop.PrePlanMode(); got != tt.wantPrePlan {
				t.Fatalf("pre-plan mode = %q, want %q", got, tt.wantPrePlan)
			}
		})
	}
}

func TestSetupRuntimeStoredPlanFromBypassRequiresStartupIsolation(t *testing.T) {
	isolateResumeRuntimeTest(t)
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "stored-plan-from-bypass-isolation"
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: "openai", Model: "stored-model",
		Mode: string(permission.ModePlan), PrePlanMode: string(permission.ModeBypassPermissions),
	}); err != nil {
		t.Fatal(err)
	}

	oldRequire := startupRequireCredentialIsolation
	t.Cleanup(func() { startupRequireCredentialIsolation = oldRequire })
	var requiredCalls []bool
	errSandboxUnavailable := errors.New("test sandbox unavailable")
	startupRequireCredentialIsolation = func(_ *sandbox.Manager, required bool) error {
		requiredCalls = append(requiredCalls, required)
		if required {
			return errSandboxUnavailable
		}
		return nil
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{resumeID: id, bare: true, noAuthWizard: true})
	if rt != nil {
		rt.Cleanup()
	}
	if !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("setupRuntime error = %v, want startup isolation failure", err)
	}
	if len(requiredCalls) != 1 || !requiredCalls[0] {
		t.Fatalf("startup isolation calls = %v, want [true]", requiredCalls)
	}
}

func TestSetupRuntimeStoredPlanFromBypassNeverStartsAuthWizard(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "stored-plan-from-bypass-no-auth-ui"
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: "openai", Model: "stored-model",
		Mode: string(permission.ModePlan), PrePlanMode: string(permission.ModeBypassPermissions),
	}); err != nil {
		t.Fatal(err)
	}

	oldTTY, oldWizard := authGateIsTTY, authGateRunWizard
	t.Cleanup(func() {
		authGateIsTTY, authGateRunWizard = oldTTY, oldWizard
	})
	authGateIsTTY = func() bool { return true }
	wizardCalls := 0
	authGateRunWizard = func() (*rtpkg.WizardResult, error) {
		wizardCalls++
		return nil, rtpkg.ErrWizardCancelled
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{resumeID: id, bare: true})
	if rt != nil {
		rt.Cleanup()
	}
	if err == nil {
		t.Fatal("missing credential unexpectedly allowed startup")
	}
	if wizardCalls != 0 {
		t.Fatalf("stored Plan<-bypass session launched auth wizard %d times", wizardCalls)
	}
}

func TestSetupRuntimeExplicitOverrideNeverAppliesStoredPermissionPosture(t *testing.T) {
	tests := []struct {
		name                string
		storedMode          permission.Mode
		storedPrePlan       string
		explicitMode        permission.Mode
		profileName         string
		wantStartupRequired bool
	}{
		{
			name:         "safer override never enters stored bypass",
			storedMode:   permission.ModeBypassPermissions,
			explicitMode: permission.ModeDefault,
		},
		{
			name:                "bypass override never enters stored safe mode",
			storedMode:          permission.ModeDefault,
			explicitMode:        permission.ModeBypassPermissions,
			wantStartupRequired: true,
		},
		{
			name:          "profile override never enters stored Plan from bypass",
			storedMode:    permission.ModePlan,
			storedPrePlan: string(permission.ModeBypassPermissions),
			explicitMode:  permission.ModeDefault,
			profileName:   "safe-permission-profile",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateResumeRuntimeTest(t)
			store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			id := "permission-single-apply-" + string(rune('a'+i))
			if err := store.WriteHeaderFull(session.Header{
				ID: id, Provider: "openai", Model: "stored-model",
				Mode: string(tt.storedMode), PrePlanMode: tt.storedPrePlan,
			}); err != nil {
				t.Fatal(err)
			}
			if tt.profileName != "" {
				agentDir := filepath.Join(config.Home(), "agents")
				if err := os.MkdirAll(agentDir, 0o755); err != nil {
					t.Fatal(err)
				}
				profile := "---\nname: " + tt.profileName + "\npermission_mode: " + string(tt.explicitMode) + "\n---\n"
				if err := os.WriteFile(filepath.Join(agentDir, tt.profileName+".md"), []byte(profile), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			oldRequire := startupRequireCredentialIsolation
			oldReconcile := startupReconcilePermissionSandbox
			t.Cleanup(func() {
				startupRequireCredentialIsolation = oldRequire
				startupReconcilePermissionSandbox = oldReconcile
			})
			var startupRequired []bool
			startupRequireCredentialIsolation = func(_ *sandbox.Manager, required bool) error {
				startupRequired = append(startupRequired, required)
				return nil
			}
			type permissionState struct {
				mode    permission.Mode
				prePlan string
			}
			var reconciled []permissionState
			startupReconcilePermissionSandbox = func(_ *sandbox.Manager, controller rtpkg.PermissionPlanController, mode permission.Mode) error {
				reconciled = append(reconciled, permissionState{mode: mode, prePlan: controller.PrePlanMode()})
				return nil
			}

			flags := &cliFlags{resumeID: id, bare: true, noAuthWizard: true, agentProfile: tt.profileName}
			if tt.profileName == "" {
				flags.mode = string(tt.explicitMode)
			}
			rt, err := setupRuntime(context.Background(), flags)
			if err != nil {
				t.Fatalf("setupRuntime: %v", err)
			}
			defer rt.Cleanup()

			if len(startupRequired) != 1 || startupRequired[0] != tt.wantStartupRequired {
				t.Fatalf("startup isolation calls = %v, want [%v]", startupRequired, tt.wantStartupRequired)
			}
			if len(reconciled) == 0 {
				t.Fatal("resume did not reconcile the final permission state")
			}
			for _, got := range reconciled {
				if got.mode != tt.explicitMode {
					t.Fatalf("transient stored permission state observed: reconciled=%+v, want only %q", reconciled, tt.explicitMode)
				}
				wantPrePlan := ""
				if tt.explicitMode == permission.ModePlan {
					wantPrePlan = string(permission.ModeDefault)
				}
				if got.prePlan != wantPrePlan {
					t.Fatalf("reconciled pre-plan mode = %q, want %q", got.prePlan, wantPrePlan)
				}
			}
		})
	}
}

func TestSetupRuntimeLegacyResumeWithoutModeUsesInvocationPlanLineage(t *testing.T) {
	isolateResumeRuntimeTest(t)
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "legacy-no-mode-invocation-plan"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "openai", Model: "stored-model"}); err != nil {
		t.Fatal(err)
	}

	oldRequire := startupRequireCredentialIsolation
	oldReconcile := startupReconcilePermissionSandbox
	t.Cleanup(func() {
		startupRequireCredentialIsolation = oldRequire
		startupReconcilePermissionSandbox = oldReconcile
	})
	startupRequireCredentialIsolation = func(_ *sandbox.Manager, required bool) error {
		if required {
			t.Fatal("invocation Plan with default lineage unexpectedly required bypass isolation")
		}
		return nil
	}
	startupReconcilePermissionSandbox = func(_ *sandbox.Manager, _ rtpkg.PermissionPlanController, _ permission.Mode) error {
		return nil
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{
		resumeID: id, mode: string(permission.ModePlan), bare: true, noAuthWizard: true,
	})
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()
	if rt.gate.Mode() != permission.ModePlan || !rt.loop.IsPlanMode() {
		t.Fatalf("permission state = gate %q, plan %v", rt.gate.Mode(), rt.loop.IsPlanMode())
	}
	if got := rt.loop.PrePlanMode(); got != string(permission.ModeDefault) {
		t.Fatalf("pre-plan mode = %q, want %q", got, permission.ModeDefault)
	}
}
