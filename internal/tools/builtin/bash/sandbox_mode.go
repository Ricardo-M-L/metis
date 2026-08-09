package bash

import "github.com/Ricardo-M-L/metis/internal/sandbox"

// Deprecated string aliases kept for source compatibility with embedders.
// Runtime state lives exclusively in an injected sandbox.Manager.

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
	SandboxModeOff         = string(sandbox.ModeOff)
	SandboxModePermissions = string(sandbox.ModePermissions)
	SandboxModeAutoAllow   = string(sandbox.ModeAutoAllow)
)

// NormalizeSandboxMode collapses user-supplied / config-supplied
// values into one of the three canonical mode strings. Unknown /
// empty values become "off" so a misspelled mode can never silently
// degrade safety to "wrapped under who-knows-what" — the worst case
// is "no sandbox", which matches the historical default.
//
// Accepted aliases (case-insensitive):
//
//	off / disabled / none           → off
//	permissions / on / enabled      → permissions
//	auto-allow / autoallow / auto   → auto-allow
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

// SandboxAvailable reports whether the unified platform backend is ready.
// Deprecated: runtime callers should use their Manager's Doctor method.
func SandboxAvailable() bool { return sandbox.Available() }
