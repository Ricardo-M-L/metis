//go:build darwin

package bash

// sandbox_darwin.go — macOS Seatbelt (sandbox-exec) wrapper for the
// Bash tool. claude-code parity for image #76's `/sandbox` modes:
//
//   off          — bash spawns directly (legacy behaviour)
//   permissions  — bash spawns under sandbox-exec with a profile that
//                  allows global file-READ but restricts file-WRITE to
//                  the metis cwd subtree + ~/.metis + system temp.
//                  Network stays unrestricted (the [sandbox.bash]
//                  network=block knob remains a separate layer).
//   auto-allow   — same sandbox profile, plus auto-approves the call
//                  through metis's permission gate (handled at the
//                  gate layer, not here).
//
// Apple has been gradually restricting sandbox-exec for years (the
// binary is officially "do not use this") but it remains functional
// and is what claude-code itself depends on. Probed on macOS 16
// (Darwin 25.4): `(deny default)(allow file-read*)` correctly
// rejects write attempts at the kernel level (EPERM), so the
// guarantee is real, not advisory.
//
// Linux / Windows: see sandbox_other.go — sandbox modes other than
// `off` reject the spawn with a clear error rather than silently
// running unsandboxed.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sandboxAvailable reports whether the platform supports the
// requested sandbox modes. macOS always returns true (we ship the
// sandbox-exec binary path); other GOOS shims return false.
func sandboxAvailable() bool { return true }

// applySandboxWrap returns a new *exec.Cmd that runs the same logical
// command through sandbox-exec when mode requires it. mode=="off"
// returns the original cmd untouched so the cheap path stays cheap.
//
// The wrapping replaces cmd.Path/Args with a sandbox-exec invocation
// that re-issues the original program + args. cmd.Env / cmd.Dir /
// cmd.Stdin etc. are preserved by mutating in place — every caller
// that already configured those continues to work without changes.
//
// cwd: the working directory the wrapped command will run in. Used
// to scope the file-write allowlist; defaults to the process cwd
// when empty.
func applySandboxWrap(ctx context.Context, cmd *exec.Cmd, mode string, cwd string) (*exec.Cmd, error) {
	mode = NormalizeSandboxMode(mode)
	if mode == SandboxModeOff {
		return cmd, nil
	}

	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return nil, fmt.Errorf("sandbox.bash.mode=%q requires sandbox-exec, which is not in PATH: %w", mode, err)
	}

	if cwd == "" {
		cwd = cmd.Dir
	}
	if cwd == "" {
		if d, err := os.Getwd(); err == nil {
			cwd = d
		}
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve cwd: %w", err)
	}
	// macOS symlinks /var → /private/var, /tmp → /private/tmp etc.
	// sandbox-exec evaluates allow paths against the physical
	// filesystem, so a cwd under /var/folders/... would be denied
	// writes by a profile that allow-listed /var/folders/... — we
	// need /private/var/folders/...
	if resolved, err := filepath.EvalSymlinks(cwdAbs); err == nil {
		cwdAbs = resolved
	}

	profile := buildSandboxProfile(cwdAbs)

	// Re-issue the original program + args via sandbox-exec. Using
	// -p (inline profile) avoids a temp file and matches what
	// claude-code does internally.
	origArgs := append([]string(nil), cmd.Args...)
	if len(origArgs) == 0 {
		origArgs = []string{cmd.Path}
	}
	newArgs := append([]string{"sandbox-exec", "-p", profile}, origArgs...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = newArgs
	// SysProcAttr / Env / Dir / Stdin / Stdout / Stderr / ExtraFiles
	// stay as-is — sandbox-exec is a thin parent that exec()s the
	// real binary, so child stdio / env propagate correctly.
	_ = ctx
	return cmd, nil
}

// buildSandboxProfile renders the Scheme-syntax Seatbelt profile
// allowed for `permissions` and `auto-allow` modes. The shape is
// deliberately conservative:
//
//   * deny everything by default
//   * allow process operations the shell needs (fork/exec/signal/wait)
//   * allow Mach lookups + sysctl reads + iokit-open (a TON of CLI
//     tools fail in opaque ways without these — `ls`, `go`, `git`
//     all use them indirectly)
//   * allow file reads everywhere (the model needs to inspect the
//     codebase)
//   * allow file writes only inside cwd, the user's ~/.metis state
//     directory, the OS temp dir, and /dev/stdout|stderr|null
//   * allow network — the [sandbox.bash] network=block knob is a
//     separate layer; double-restricting here would surprise users
//     who didn't ask for it
//
// Note the (allow ...) clauses come AFTER (deny default) — Seatbelt
// evaluates rules in order so denies must come first, then allows.
func buildSandboxProfile(cwd string) string {
	home, _ := os.UserHomeDir()
	tmp := os.TempDir()
	// Same symlink-resolve dance the wrap path does — tmp / home /
	// cwd commonly traverse /private symlinks on macOS, and
	// sandbox-exec evaluates paths against the physical filesystem.
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	if home != "" {
		if r, err := filepath.EvalSymlinks(home); err == nil {
			home = r
		}
	}

	// Each subpath has to be quoted; backslashes / quotes inside
	// the path must be escaped. Paths with "(" ")" are theoretically
	// problematic but extremely rare for cwd / temp / home.
	quote := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return `"` + s + `"`
	}

	allows := []string{
		`(allow process-exec)`,
		`(allow process-fork)`,
		`(allow signal (target self))`,
		`(allow mach-lookup)`,
		`(allow sysctl-read)`,
		`(allow iokit-open)`,
		`(allow file-read*)`,
		// network: allow everything; orthogonal knob handles
		// outbound blocking via $http_proxy injection.
		`(allow network*)`,
		// file-write allowlist (subpath = the dir + everything under it)
		fmt.Sprintf(`(allow file-write* (subpath %s))`, quote(cwd)),
		fmt.Sprintf(`(allow file-write* (subpath %s))`, quote(tmp)),
		`(allow file-write* (literal "/dev/null"))`,
		`(allow file-write* (literal "/dev/stdout"))`,
		`(allow file-write* (literal "/dev/stderr"))`,
		`(allow file-write-data (literal "/dev/tty"))`,
	}
	if home != "" {
		metisDir := filepath.Join(home, ".metis")
		allows = append(allows,
			fmt.Sprintf(`(allow file-write* (subpath %s))`, quote(metisDir)))
	}

	return "(version 1)\n(deny default)\n" + strings.Join(allows, "\n") + "\n"
}
