package tui

// /sandbox — manage the macOS Seatbelt (sandbox-exec) wrapper applied
// to Bash subprocesses. Implements the same three-option choice
// claude-code surfaces in its image #76 menu:
//
//   off          — direct spawn, legacy default
//   permissions  — wrap in sandbox-exec; gate still asks/auto/bypass
//   auto-allow   — wrap + auto-approve the gate (sandbox is the
//                  bound, not the user click)
//
// The slash command writes to the in-process runtime override
// (bash.SetRuntimeSandboxMode) so the change takes effect on the
// VERY NEXT Bash call — no restart. Persistence to ~/.metis/config.toml
// is intentionally NOT done here; users who want a setting to survive
// restarts edit [bash.sandbox] mode = "..." in config.toml directly
// (the runtime override always wins, so the next /sandbox call shows
// the override they set even after a config edit).
//
// On non-darwin platforms only `off` is supported; the command
// rejects the other modes with a clear message rather than silently
// running unsandboxed.

import (
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

func cmdSandbox(_ *REPL, args string) string {
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "" || args == "status" {
		return sandboxStatus()
	}
	if args == "help" {
		return sandboxHelp()
	}

	// Treat any other argument as a mode-set request.
	canonical := bash.NormalizeSandboxMode(args)
	// NormalizeSandboxMode silently collapses unknown values to
	// "off" — for the slash command we'd rather REJECT typos so a
	// user typing `/sandbox permission` (no -s) doesn't think they
	// set permissions when they actually disabled the sandbox.
	if canonical == bash.SandboxModeOff && args != "off" && args != "disabled" && args != "none" {
		return fmt.Sprintf("sandbox: unknown mode %q. usage: /sandbox [status | off | permissions | auto-allow]", args)
	}

	// Platform check before we accept anything other than off — on
	// non-macOS the wrapper would error on the next Bash call anyway,
	// but surfacing it here gives the user a single clean message
	// instead of seeing the failure on every command.
	if canonical != bash.SandboxModeOff && !bash.SandboxAvailable() {
		return "sandbox: only `off` is supported on this platform (macOS-only today; Linux landlock / Windows job-objects pending)"
	}

	bash.SetRuntimeSandboxMode(canonical)
	return fmt.Sprintf("sandbox: mode set to %q (effective immediately for new Bash calls)\n  • persists for this session only — edit ~/.metis/config.toml [bash.sandbox] mode = %q to survive restarts",
		canonical, canonical)
}

func sandboxStatus() string {
	override := bash.RuntimeSandboxMode()
	available := bash.SandboxAvailable()
	var b strings.Builder
	if override != "" {
		fmt.Fprintf(&b, "sandbox: %s (runtime override active)\n", override)
	} else {
		b.WriteString("sandbox: using config.toml [bash.sandbox] mode (default: off)\n")
	}
	if !available {
		b.WriteString("  • this platform supports `off` only (macOS-only today)\n")
	}
	b.WriteString("\nmodes:\n")
	b.WriteString("  off          — direct spawn (current legacy default)\n")
	b.WriteString("  permissions  — wrap in sandbox-exec; cwd/temp/~/.metis writable, rest read-only; gate still asks\n")
	b.WriteString("  auto-allow   — wrap + auto-approve the permission gate (sandbox bounds the blast)\n")
	b.WriteString("\nusage: /sandbox [status | off | permissions | auto-allow]")
	return b.String()
}

func sandboxHelp() string {
	return strings.TrimSpace(`
/sandbox — macOS Seatbelt wrapper for the Bash tool.

  /sandbox             show current mode + options
  /sandbox status      same as no-arg
  /sandbox off         disable the wrapper (legacy default)
  /sandbox permissions wrap bash via sandbox-exec; gate keeps asking
  /sandbox auto-allow  wrap + auto-approve so the sandbox is the only bound

Aliases:
  on / enabled         → permissions
  auto / autoallow     → auto-allow
  disabled / none      → off

The setting takes effect on the next Bash call — no restart needed.
To make it stick across restarts, edit ~/.metis/config.toml:

  [bash.sandbox]
  mode = "permissions"
`)
}
