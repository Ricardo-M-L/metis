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
// its Plan lineage. The transition coordinator keeps both reads on one side of
// every coordinated mode transition; otherwise entering Plan can expose the
// new lineage before the Gate commits Plan, while exiting can clear lineage
// before the Gate commits the restored mode. Callers that optimistically apply
// a live setting must keep this complete state so a persistence failure cannot
// rewrite Plan's exit target to the temporary preview mode.
func CapturePermissionModeState(gate *permission.Gate, controller PermissionPlanController) (PermissionModeState, error) {
	if gate == nil {
		return PermissionModeState{}, errors.New("permission gate is unavailable")
	}
	if isNilPermissionPlanController(controller) {
		controller = nil
	}
	var state PermissionModeState
	err := gate.RunModeTransition(func() error {
		prePlan := ""
		if controller != nil {
			prePlan = controller.PrePlanMode()
		}
		mode, validatedPrePlan, _, err := ValidateRestoredPermissionState(string(gate.Mode()), prePlan, permission.ModeDefault)
		if err != nil {
			return err
		}
		state = PermissionModeState{Mode: mode, PrePlanMode: validatedPrePlan}
		return nil
	})
	return state, err
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
// coordinates the permission-owned sandbox posture with the Gate. Entering
// bypassPermissions installs credential isolation before removing prompts.
// Entering fullAccess commits the Gate first and only then disables sandboxing,
// so a concurrent call can observe a temporarily stricter sandbox but never an
// unsandboxed process under an older auto-approval posture. Leaving either
// permissive mode commits the safer Gate before restoring the configured
// sandbox selection.
func ApplyPermissionMode(gate *permission.Gate, controller PermissionPlanController, manager *sandbox.Manager, mode permission.Mode) error {
	if gate == nil {
		return errors.New("permission gate is unavailable")
	}
	mode = permission.CanonicalMode(string(mode))
	return gate.RunModeTransition(func() error {
		return applyPermissionModeTransition(gate, controller, manager, mode)
	})
}

// applyPermissionModeTransition runs while Gate keeps tool checks fail-closed
// and serializes other ApplyPermissionMode callers. Direct SetMode calls remain
// responsive; SetModeAndWait plus the committed-mode checks below turn those
// calls into an explicit supersession instead of letting this older transition
// write a stale sandbox posture after the newer call returned.
func applyPermissionModeTransition(gate *permission.Gate, controller PermissionPlanController, manager *sandbox.Manager, mode permission.Mode) error {
	current := gate.Mode()
	if isNilPermissionPlanController(controller) {
		controller = nil
	}
	previousPrePlan := ""
	if controller != nil {
		previousPrePlan = controller.PrePlanMode()
	}
	requiresIsolation, requiresFullAccess := permissionModeSandboxPosture(current, mode, controller)
	requiresManager := requiresIsolation || requiresFullAccess || current == permission.ModeBypassPermissions || current == permission.ModeFullAccess
	if requiresManager && manager == nil {
		return fmt.Errorf("%s requires a runtime sandbox manager", mode)
	}
	if requiresFullAccess {
		if err := manager.PreflightFullAccess(); err != nil {
			return err
		}
	}
	// bypassPermissions must establish its credential floor before the Gate can
	// suppress prompts. fullAccess uses the reverse order below because its
	// intermediate state is safe only while the old sandbox is still active.
	if requiresIsolation {
		if err := manager.SetPermissionPosture(true, false); err != nil {
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
	gate.SetModeAndWait(mode)
	if committed := gate.Mode(); committed != mode {
		synchronizePermissionController(controller, committed)
		if _, err := settlePermissionSandbox(gate, manager, controller); err != nil {
			return fmt.Errorf("permission mode transition to %s was superseded by %s; reconcile committed posture: %w", mode, committed, err)
		}
		return fmt.Errorf("permission mode transition to %s was superseded by %s", mode, committed)
	}
	if controller != nil {
		controller.SetPlanMode(mode == permission.ModePlan)
	}

	// The production listener normally installed the exact posture while the
	// callback drain ran. Do not repeat that write: an older Apply used to land
	// here after a newer transition and overwrite the listener's safe result.
	// Reduced embedders have no such listener, so a state mismatch is the signal
	// to install the fallback posture while the outer transition guard remains.
	if !permissionSandboxPostureMatches(manager, controller, mode) {
		if err := ReconcilePermissionSandbox(manager, controller, mode); err != nil {
			if controller != nil {
				controller.SetPrePlanMode(previousPrePlan)
				controller.SetPlanMode(current == permission.ModePlan)
			}
			gate.SetModeAndWait(current)
			return fmt.Errorf("apply sandbox posture for %s: %w", mode, err)
		}
	}
	if committed := gate.Mode(); committed != mode {
		synchronizePermissionController(controller, committed)
		if _, err := settlePermissionSandbox(gate, manager, controller); err != nil {
			return fmt.Errorf("permission mode transition to %s was superseded by %s; reconcile committed posture: %w", mode, committed, err)
		}
		return fmt.Errorf("permission mode transition to %s was superseded by %s", mode, committed)
	}
	return nil
}

// settlePermissionSandbox follows direct/re-entrant SetMode supersessions until
// one mode remains stable across its matching manager write. Three passes are
// enough for the normal requested->listener-fail-closed sequence while keeping
// an adversarial stream of direct mutations bounded and fail-closed by Gate's
// outer transition hold.
func settlePermissionSandbox(gate *permission.Gate, manager *sandbox.Manager, controller PermissionPlanController) (permission.Mode, error) {
	for range 3 {
		committed := gate.Mode()
		synchronizePermissionController(controller, committed)
		if !permissionSandboxPostureMatches(manager, controller, committed) {
			if err := ReconcilePermissionSandbox(manager, controller, committed); err != nil {
				return committed, err
			}
		}
		if gate.Mode() == committed {
			return committed, nil
		}
	}
	return gate.Mode(), errors.New("permission mode did not settle after concurrent updates")
}

func permissionSandboxPostureMatches(manager *sandbox.Manager, controller PermissionPlanController, mode permission.Mode) bool {
	isolation, fullAccess := requiredPermissionSandboxPosture(controller, mode)
	if manager == nil {
		return !isolation && !fullAccess
	}
	state := manager.State()
	return state.CredentialIsolationRequired == isolation && state.FullAccessRequired == fullAccess
}

func requiredPermissionSandboxPosture(controller PermissionPlanController, mode permission.Mode) (isolation, fullAccess bool) {
	isolation = mode == permission.ModeBypassPermissions
	fullAccess = mode == permission.ModeFullAccess
	if mode == permission.ModePlan && !isNilPermissionPlanController(controller) {
		previous, ok := permission.ParseMode(controller.PrePlanMode())
		isolation = ok && previous == permission.ModeBypassPermissions
	}
	return isolation, fullAccess
}

func synchronizePermissionController(controller PermissionPlanController, committed permission.Mode) {
	if isNilPermissionPlanController(controller) {
		return
	}
	if committed != permission.ModePlan {
		controller.SetPrePlanMode("")
	}
	controller.SetPlanMode(committed == permission.ModePlan)
}

// ReconcilePermissionSandbox covers internal Gate.SetMode callers such as the
// Enter/ExitPlanMode tools and session restoration. Public UI paths should use
// ApplyPermissionMode so an unavailable backend can be reported before the
// gate changes.
func ReconcilePermissionSandbox(manager *sandbox.Manager, controller PermissionPlanController, mode permission.Mode) error {
	if isNilPermissionPlanController(controller) {
		controller = nil
	}
	requiredIsolation, requiredFullAccess := requiredPermissionSandboxPosture(controller, mode)
	if manager == nil {
		if requiredIsolation || requiredFullAccess {
			return fmt.Errorf("%s requires a runtime sandbox manager", mode)
		}
		return nil
	}
	return manager.SetPermissionPosture(requiredIsolation, requiredFullAccess)
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

// ValidateRestoredPermissionStateFromSource applies the session-store trust
// boundary after structural validation. An untrusted store may retain normal
// session UX such as plan/dontAsk, but it cannot mint the host-unrestricted
// fullAccess posture, hide fullAccess behind Plan's exit lineage, or claim that
// a filtered direct fullAccess value was a stored authorization decision.
//
// The caller separately filters AlwaysAllow from an untrusted header. Keeping
// that decision beside this one makes every resume frontend use one provenance
// bit for the complete persisted authorization state.
func ValidateRestoredPermissionStateFromSource(rawMode, rawPrePlan string, fallback permission.Mode, trustStoredPermissions bool) (permission.Mode, string, bool, error) {
	mode, prePlan, hasStoredMode, err := ValidateRestoredPermissionState(rawMode, rawPrePlan, fallback)
	if err != nil || trustStoredPermissions || !hasStoredMode {
		return mode, prePlan, hasStoredMode, err
	}

	fallback = permission.CanonicalMode(string(fallback))
	if mode == permission.ModeFullAccess {
		if fallback == permission.ModePlan {
			return fallback, string(permission.ModeDefault), false, nil
		}
		return fallback, "", false, nil
	}
	if mode == permission.ModePlan && prePlan == string(permission.ModeFullAccess) {
		lineage := fallback
		if lineage == permission.ModePlan {
			lineage = permission.ModeDefault
		}
		return mode, string(lineage), true, nil
	}
	return mode, prePlan, hasStoredMode, nil
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
	if !required && mode != permission.ModeFullAccess {
		return nil
	}
	if manager == nil {
		return fmt.Errorf("%s requires a runtime sandbox manager", mode)
	}
	if mode == permission.ModeFullAccess {
		return manager.PreflightFullAccess()
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

func permissionModeSandboxPosture(current, next permission.Mode, controller PermissionPlanController) (credentialIsolation, fullAccess bool) {
	if next == permission.ModeFullAccess {
		return false, true
	}
	if next == permission.ModeBypassPermissions {
		return true, false
	}
	if next != permission.ModePlan {
		return false, false
	}
	if current == permission.ModeBypassPermissions {
		return true, false
	}
	if controller == nil {
		return false, false
	}
	previous, ok := permission.ParseMode(controller.PrePlanMode())
	return ok && previous == permission.ModeBypassPermissions, false
}
