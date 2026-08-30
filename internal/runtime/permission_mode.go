package runtime

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// PermissionPlanController is the small part of agent.Loop needed to keep the
// permission gate, plan lineage, and process credential boundary in sync.
type PermissionPlanController interface {
	PrePlanMode() string
	SetPrePlanMode(string)
	SetPlanMode(bool)
}

// PermissionModeState is the complete user-visible permission posture. Plan
// mode is an overlay, so the gate mode alone is not a sufficient snapshot: the
// mode to restore when Plan exits is part of the same state.
type PermissionModeState struct {
	Mode        permission.Mode
	PrePlanMode string
}

// CapturePermissionModeState validates and snapshots the gate together with
// its Plan lineage. Callers that optimistically apply a live setting must keep
// this complete state so a persistence failure cannot rewrite Plan's exit
// target to the temporary preview mode.
func CapturePermissionModeState(gate *permission.Gate, controller PermissionPlanController) (PermissionModeState, error) {
	if gate == nil {
		return PermissionModeState{}, errors.New("permission gate is unavailable")
	}
	if isNilPermissionPlanController(controller) {
		controller = nil
	}
	prePlan := ""
	if controller != nil {
		prePlan = controller.PrePlanMode()
	}
	mode, validatedPrePlan, _, err := ValidateRestoredPermissionState(string(gate.Mode()), prePlan, permission.ModeDefault)
	if err != nil {
		return PermissionModeState{}, err
	}
	return PermissionModeState{Mode: mode, PrePlanMode: validatedPrePlan}, nil
}

// RestorePermissionModeState replays a complete permission snapshot through
// the normal transition function. Replaying the underlying mode first is what
// makes re-entering Plan capture the original lineage instead of whichever
// temporary setting happened to be active when persistence failed.
func RestorePermissionModeState(state PermissionModeState, apply func(permission.Mode) error) error {
	if apply == nil {
		return errors.New("permission transition is unavailable")
	}
	mode, prePlan, _, err := ValidateRestoredPermissionState(string(state.Mode), state.PrePlanMode, permission.ModeDefault)
	if err != nil {
		return err
	}
	if mode == permission.ModePlan {
		previous, _ := permission.ParseMode(prePlan) // validated above
		if err := apply(previous); err != nil {
			return fmt.Errorf("restore pre-plan permission mode: %w", err)
		}
	}
	if err := apply(mode); err != nil {
		return fmt.Errorf("restore permission mode: %w", err)
	}
	return nil
}

// SynchronizeRestoredPermissionState repairs the controller after a Gate
// session reset. A mode listener may synchronously fail closed (for example,
// Plan inherited bypass isolation but the OS sandbox became unavailable), so
// the committed Gate mode—not the requested persisted mode—is authoritative.
func SynchronizeRestoredPermissionState(gate *permission.Gate, controller PermissionPlanController, requestedPrePlan string) permission.Mode {
	if gate == nil {
		return permission.ModeDefault
	}
	committed := gate.Mode()
	if isNilPermissionPlanController(controller) {
		return committed
	}
	prePlan := ""
	if committed == permission.ModePlan {
		previous, ok := permission.ParseMode(requestedPrePlan)
		if !ok || previous == permission.ModePlan {
			previous = permission.ModeDefault
		}
		prePlan = string(previous)
	}
	controller.SetPrePlanMode(prePlan)
	controller.SetPlanMode(committed == permission.ModePlan)
	return committed
}

// ApplyPermissionMode is the single user-driven permission transition. It
// installs the credential-isolation floor before committing an unattended
// posture, so another goroutine can never observe bypassPermissions with an
// unsandboxed subprocess boundary. Leaving unattended mode commits the safer
// gate first and only then restores the user's sandbox selection.
func ApplyPermissionMode(gate *permission.Gate, controller PermissionPlanController, manager *sandbox.Manager, mode permission.Mode) error {
	if gate == nil {
		return errors.New("permission gate is unavailable")
	}
	mode = permission.CanonicalMode(string(mode))
	current := gate.Mode()
	if isNilPermissionPlanController(controller) {
		controller = nil
	}
	requiresIsolation := permissionModeRequiresCredentialIsolation(current, mode, controller)
	if requiresIsolation {
		if manager == nil {
			return errors.New("bypassPermissions requires a runtime sandbox manager")
		}
		if err := manager.RequireCredentialIsolation(true); err != nil {
			return err
		}
	}

	if controller != nil {
		if mode == permission.ModePlan {
			if current != permission.ModePlan {
				controller.SetPrePlanMode(string(current))
			}
		} else {
			controller.SetPrePlanMode("")
		}
	}
	gate.SetMode(mode)
	if controller != nil {
		controller.SetPlanMode(mode == permission.ModePlan)
	}

	if !requiresIsolation && manager != nil {
		if err := manager.RequireCredentialIsolation(false); err != nil {
			return fmt.Errorf("restore configured sandbox after permission change: %w", err)
		}
	}
	return nil
}

// ReconcilePermissionSandbox covers internal Gate.SetMode callers such as the
// Enter/ExitPlanMode tools and session restoration. Public UI paths should use
// ApplyPermissionMode so an unavailable backend can be reported before the
// gate changes.
func ReconcilePermissionSandbox(manager *sandbox.Manager, controller PermissionPlanController, mode permission.Mode) error {
	if isNilPermissionPlanController(controller) {
		controller = nil
	}
	if manager == nil {
		if mode == permission.ModeBypassPermissions {
			return errors.New("bypassPermissions requires a runtime sandbox manager")
		}
		return nil
	}
	required := mode == permission.ModeBypassPermissions
	if mode == permission.ModePlan && controller != nil {
		previous, ok := permission.ParseMode(controller.PrePlanMode())
		required = ok && previous == permission.ModeBypassPermissions
	}
	return manager.RequireCredentialIsolation(required)
}

// ValidateRestoredPermissionState canonicalizes the two permission fields in
// a session header and rejects impossible or tampered combinations. Legacy
// plan sessions did not persist their lineage; they safely fall back to
// default rather than guessing that they came from bypassPermissions.
func ValidateRestoredPermissionState(rawMode, rawPrePlan string, fallback permission.Mode) (permission.Mode, string, bool, error) {
	if rawMode == "" {
		if rawPrePlan != "" {
			return "", "", false, errors.New("session permission state has pre_plan_mode without mode")
		}
		return fallback, "", false, nil
	}
	mode, ok := permission.ParseMode(rawMode)
	if !ok {
		return "", "", false, fmt.Errorf("session permission mode %q is invalid", rawMode)
	}
	if mode != permission.ModePlan {
		if rawPrePlan != "" {
			return "", "", false, fmt.Errorf("session pre_plan_mode is only valid with plan mode (mode %q)", mode)
		}
		return mode, "", true, nil
	}
	if rawPrePlan == "" {
		return mode, string(permission.ModeDefault), true, nil
	}
	prePlan, ok := permission.ParseMode(rawPrePlan)
	if !ok || prePlan == permission.ModePlan {
		return "", "", false, fmt.Errorf("session pre_plan_mode %q is invalid", rawPrePlan)
	}
	return mode, string(prePlan), true, nil
}

// PreflightRestoredPermissionState checks the process isolation requirement
// before a session switch mutates its source session. Plan sessions inherit
// the requirement when they were entered from bypassPermissions.
func PreflightRestoredPermissionState(manager *sandbox.Manager, mode permission.Mode, prePlan string) error {
	required := mode == permission.ModeBypassPermissions
	if mode == permission.ModePlan {
		previous, ok := permission.ParseMode(prePlan)
		if !ok || previous == permission.ModePlan {
			return fmt.Errorf("invalid restored plan lineage %q", prePlan)
		}
		required = previous == permission.ModeBypassPermissions
	}
	if !required {
		return nil
	}
	if manager == nil {
		return errors.New("bypassPermissions requires a runtime sandbox manager")
	}
	return manager.PreflightCredentialIsolation()
}

// An interface containing a nil *agent.Loop is not itself nil. TUI tests and
// reduced embedders legitimately operate without a loop, so normalize typed
// nil controllers before invoking their methods.
func isNilPermissionPlanController(controller PermissionPlanController) bool {
	if controller == nil {
		return true
	}
	v := reflect.ValueOf(controller)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func permissionModeRequiresCredentialIsolation(current, next permission.Mode, controller PermissionPlanController) bool {
	if next == permission.ModeBypassPermissions {
		return true
	}
	if next != permission.ModePlan {
		return false
	}
	if current == permission.ModeBypassPermissions {
		return true
	}
	if controller == nil {
		return false
	}
	previous, ok := permission.ParseMode(controller.PrePlanMode())
	return ok && previous == permission.ModeBypassPermissions
}
