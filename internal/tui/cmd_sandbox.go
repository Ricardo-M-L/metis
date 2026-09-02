package tui

// /sandbox manages the per-runtime OS sandbox shared by every
// model-controlled command entry point. Permission mode and sandbox mode are
// deliberately orthogonal for ordinary modes: bypassPermissions can remove
// approval prompts, but it never disables an enabled kernel sandbox.
// fullAccess is the explicit exception and forces direct host execution.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

type sandboxManagerProvider interface {
	SandboxManager() *sandbox.Manager
}

func replSandboxManager(r *REPL) *sandbox.Manager {
	if r == nil || r.Loop == nil || r.Loop.Registry == nil {
		if r != nil {
			return r.sandbox
		}
		return nil
	}
	if r.sandbox != nil {
		return r.sandbox
	}
	tool, ok := r.Loop.Registry.Get("Bash")
	if !ok {
		return nil
	}
	provider, ok := tool.(sandboxManagerProvider)
	if !ok {
		return nil
	}
	return provider.SandboxManager()
}

func cmdSandbox(r *REPL, args string) string {
	manager := replSandboxManager(r)
	if manager == nil {
		return "sandbox: runtime manager is unavailable"
	}

	arg := strings.ToLower(strings.TrimSpace(args))
	switch arg {
	case "", "status":
		return sandboxStatus(manager)
	case "help":
		return sandboxHelp()
	case "doctor":
		return sandboxDoctor(manager)
	case "reset", "config":
		if replRequiresCredentialIsolation(r) {
			return "sandbox: cannot clear the runtime boundary while bypassPermissions is active; switch permission mode first"
		}
		manager.ClearRuntimeMode()
		return fmt.Sprintf("sandbox: runtime override cleared; effective mode is %q from [tools.bash.sandbox]", manager.EffectiveMode())
	}

	canonical, ok := interactiveSandboxMode(arg)
	if !ok {
		return fmt.Sprintf("sandbox: unknown mode %q. usage: /sandbox [status | doctor | reset | off | permissions | auto-allow]", arg)
	}
	if canonical == sandbox.ModeOff && replRequiresCredentialIsolation(r) {
		return "sandbox: cannot disable credential isolation while bypassPermissions is active; switch permission mode first"
	}
	if canonical != sandbox.ModeOff {
		diagnostic := manager.Doctor()
		if !diagnostic.Available {
			return fmt.Sprintf("sandbox: cannot enable %q: %v (fail-closed; mode unchanged)", canonical, diagnostic.Err)
		}
	}
	if err := manager.SetRuntimeMode(string(canonical)); err != nil {
		return "sandbox: mode unchanged: " + err.Error()
	}
	if manager.State().FullAccessRequired {
		return fmt.Sprintf(
			"sandbox: runtime override set to %q, but fullAccess keeps the effective process sandbox off; switch permission mode to reactivate this boundary",
			canonical,
		)
	}
	return fmt.Sprintf(
		"sandbox: mode set to %q for this runtime (effective immediately for all model-controlled subprocesses: Bash, RunCode, Workflow, Git/scope, LSP, Monitor, MCP/Computer Use, Skills and custom commands)\n  • bypassPermissions does not disable this boundary\n  • persist with [tools.bash.sandbox] mode = %q in ~/.metis/config.toml",
		canonical, canonical,
	)
}

func replRequiresCredentialIsolation(r *REPL) bool {
	if r == nil || r.Gate == nil {
		return false
	}
	mode := r.Gate.Mode()
	if mode == permission.ModeBypassPermissions {
		return true
	}
	if mode != permission.ModePlan || r.Loop == nil {
		return false
	}
	previous, ok := permission.ParseMode(r.Loop.PrePlanMode())
	return ok && previous == permission.ModeBypassPermissions
}

func interactiveSandboxMode(arg string) (sandbox.Mode, bool) {
	switch arg {
	case "off", "disabled", "none":
		return sandbox.ModeOff, true
	case "permissions", "on", "enabled":
		return sandbox.ModePermissions, true
	case "auto-allow", "autoallow", "auto":
		return sandbox.ModeAutoAllow, true
	default:
		return "", false
	}
}

func sandboxStatus(manager *sandbox.Manager) string {
	state := manager.State()
	diagnostic := manager.Doctor()
	var b strings.Builder
	fmt.Fprintf(&b, "sandbox: %s", state.Effective)
	if state.HasRuntimeOverride {
		fmt.Fprintf(&b, " (runtime override; configured: %s)", state.Configured)
	} else {
		b.WriteString(" (from [tools.bash.sandbox])")
	}
	if state.CredentialIsolationRequired {
		b.WriteString("\n  • credential isolation: enforced by bypassPermissions")
	}
	if state.FullAccessRequired {
		b.WriteString("\n  ⚠ full access: fullAccess forces the process sandbox off; model-controlled subprocesses run directly on the host")
	}
	fmt.Fprintf(&b, "\n  • backend: %s on %s", diagnostic.Backend, diagnostic.Platform)
	if diagnostic.Available {
		fmt.Fprintf(&b, " (%s)", diagnostic.Executable)
	} else {
		fmt.Fprintf(&b, " (unavailable: %v)", diagnostic.Err)
	}
	fmt.Fprintf(&b, "\n  • network: %s", manager.NetworkPolicy())
	if manager.NetworkPolicy() == sandbox.NetworkBlock && diagnostic.Backend == "bubblewrap" {
		b.WriteString(" (IP namespace; common container sockets masked; abstract AF_UNIX is not seccomp-filtered)")
	}
	if temp := manager.TempDir(); temp != "" {
		fmt.Fprintf(&b, "\n  • private temp: %s", temp)
	}
	if state.FullAccessRequired {
		b.WriteString("\n  • writable: unrestricted by the process sandbox (host OS permissions still apply)")
		b.WriteString("\n  • coverage: bypassed for Bash, RunCode, Workflow, Git/scope, LSP, Monitor, MCP/Computer Use, Skills and custom-command subprocesses")
	} else {
		b.WriteString("\n  • writable: effective cwd/worktree + private temp; Metis control files and Git hooks/config stay protected")
		b.WriteString("\n  • coverage: Bash, RunCode, Workflow, Git/scope, LSP, Monitor, MCP/Computer Use, Skills and custom-command subprocesses")
	}
	if sandboxCwdIsHome() && state.Effective != sandbox.ModeOff {
		b.WriteString("\n  ⚠ workspace root is your home directory, so the writable boundary is broad; start Metis inside the project directory for tighter isolation")
	}
	b.WriteString("\n\nmodes:\n")
	b.WriteString("  off          — direct host spawn\n")
	b.WriteString("  permissions  — OS sandbox; permission gate still applies\n")
	b.WriteString("  auto-allow   — same OS sandbox; ordinary Ask prompts auto-allow (explicit deny/plan still win)\n")
	b.WriteString("\nusage: /sandbox [status | doctor | reset | off | permissions | auto-allow]")
	return b.String()
}

func sandboxCwdIsHome() bool {
	cwd, cwdErr := os.Getwd()
	home, homeErr := os.UserHomeDir()
	if cwdErr != nil || homeErr != nil || home == "" {
		return false
	}
	cwd, _ = filepath.Abs(cwd)
	home, _ = filepath.Abs(home)
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	return filepath.Clean(cwd) == filepath.Clean(home)
}

func sandboxDoctor(manager *sandbox.Manager) string {
	d := manager.Doctor()
	status := "unavailable"
	if d.Available {
		status = "ready"
	}
	out := fmt.Sprintf("sandbox doctor: %s\n  platform: %s\n  backend: %s\n  executable: %s", status, d.Platform, d.Backend, d.Executable)
	if d.Err != nil {
		out += "\n  error: " + d.Err.Error()
	}
	return out
}

func sandboxHelp() string {
	return strings.TrimSpace(`
/sandbox — per-runtime operating-system sandbox for model-controlled commands.

  /sandbox             show effective/configured mode and backend
  /sandbox status      same as no-arg
  /sandbox doctor      diagnose sandbox backend/dependency
  /sandbox reset       clear the session override and use config
  /sandbox off         direct host execution
  /sandbox permissions enable OS isolation; permission gate still applies
  /sandbox auto-allow  enable OS isolation and auto-allow ordinary Ask prompts

Aliases: on/enabled -> permissions; auto/autoallow -> auto-allow;
disabled/none -> off.

The setting applies to new commands in this runtime. To persist it:

  [tools.bash.sandbox]
  mode = "permissions"
  network = "block"

bypassPermissions only changes approval prompts; it does not disable an
enabled OS sandbox. The boundary covers all model-controlled subprocesses,
including Bash, RunCode, Workflow, Git/scope, LSP, Monitor, MCP/Computer Use,
Skills, and custom commands. fullAccess is the explicit exception: it forces
the effective process sandbox off until you switch permission mode.`)
}
