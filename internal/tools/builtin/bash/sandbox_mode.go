package bash

// sandbox_mode.go — shared (build-tag-free) helpers for the
// SandboxMode enum. Lives in its own file so darwin / non-darwin
// sandbox_*.go can both reference it without duplicating the
// canonical names + the NormalizeSandboxMode tolerance rules.

// SandboxModeOff disables the macOS Seatbelt wrapper entirely and
// runs the bash subprocess directly. Backwards-compatible default —
// every metis session prior to 2026-05-26 effectively ran in this
// mode whether it was configured or not.
//
// SandboxModePermissions wraps the bash subprocess in sandbox-exec
// with a profile that allows global file-READ but limits file-WRITE
// to the cwd subtree + ~/.metis + os.TempDir + the std I/O dev
// nodes. Existing metis permission gate (ask/auto/bypass/plan) still
// runs in front, unchanged.
//
// SandboxModeAutoAllow is permissions + auto-approve the call
// through the permission gate. Matches claude-code's image #76
// option 1 ("Sandbox BashTool, with auto-allow"). Useful when the
// user trusts the sandbox to bound the blast radius and doesn't
// want to manually approve every command.
const (
	SandboxModeOff         = "off"
	SandboxModePermissions = "permissions"
	SandboxModeAutoAllow   = "auto-allow"
)

// NormalizeSandboxMode collapses user-supplied / config-supplied
// values into one of the three canonical mode strings. Unknown /
// empty values become "off" so a misspelled mode can never silently
// degrade safety to "wrapped under who-knows-what" — the worst case
// is "no sandbox", which matches the historical default.
//
// Accepted aliases (case-insensitive):
//   off / disabled / none           → off
//   permissions / on / enabled      → permissions
//   auto-allow / autoallow / auto   → auto-allow
func NormalizeSandboxMode(mode string) string {
	switch mode {
	case "", SandboxModeOff, "disabled", "none":
		return SandboxModeOff
	case SandboxModePermissions, "on", "enabled":
		return SandboxModePermissions
	case SandboxModeAutoAllow, "autoallow", "auto":
		return SandboxModeAutoAllow
	default:
		return SandboxModeOff
	}
}

// SandboxModeAutoApprovesGate reports whether the given mode tells
// the permission gate to skip its dialog and run immediately. Only
// SandboxModeAutoAllow does — Off + Permissions still ask normally.
func SandboxModeAutoApprovesGate(mode string) bool {
	return NormalizeSandboxMode(mode) == SandboxModeAutoAllow
}

// SandboxAvailable reports whether the runtime supports sandbox
// modes other than "off". Exported wrapper around the per-platform
// sandboxAvailable() so callers outside this package (e.g. the
// /sandbox slash command) can show a "not supported on this OS"
// message before letting the user pick.
func SandboxAvailable() bool { return sandboxAvailable() }

// runtimeSandboxMode is the in-process override applied on top of
// whatever the [bash.sandbox] mode in config.toml says. Set via the
// /sandbox slash command so users can flip modes without editing
// the config + restarting metis. Empty string means "no override —
// honour config.toml verbatim". Reset to empty by passing "" to
// SetRuntimeSandboxMode.
//
// Stored as a string (not a typed enum) because the only
// constraint is "valid sandbox mode or empty"; NormalizeSandboxMode
// collapses everything else to empty at read time.
var runtimeSandboxMode string

// SetRuntimeSandboxMode installs a session-scoped sandbox-mode
// override. Pass "" to clear; pass "off" / "permissions" /
// "auto-allow" (or an alias NormalizeSandboxMode accepts) to set.
// Invalid values collapse to empty (no override) so a typo can't
// silently degrade safety.
func SetRuntimeSandboxMode(mode string) {
	if mode == "" {
		runtimeSandboxMode = ""
		return
	}
	canonical := NormalizeSandboxMode(mode)
	runtimeSandboxMode = canonical
}

// RuntimeSandboxMode reports the current in-process override. Empty
// string when none is active. /sandbox status uses this to show the
// difference between "config.toml mode" and "session override".
func RuntimeSandboxMode() string {
	return runtimeSandboxMode
}

// effectiveMode resolves the final mode the wrapper / gate uses,
// preferring the runtime override over the config value. Centralised
// so all callers (CanUse, applySandboxWrap) see the same answer
// without each repeating the precedence rule.
func effectiveMode(configMode string) string {
	if runtimeSandboxMode != "" {
		return runtimeSandboxMode
	}
	return NormalizeSandboxMode(configMode)
}
