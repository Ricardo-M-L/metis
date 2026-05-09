//go:build linux

package jobs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// oom_linux.go — protect metis from being kernel-killed when one of
// its bash subprocesses goes runaway. Adjusts /proc/self/oom_score_adj
// in the child so the OOM killer always picks the subprocess first
// when memory is tight.
//
// Why this matters: a `make build` in a memory-strapped CI box can
// balloon to several GB. Linux's OOM killer scans processes by RSS
// and ranks them by oom_score (a number derived from RSS plus an
// adjustable +/- bias in /proc/<pid>/oom_score_adj, range -1000..1000).
// Without this hook, the highest-RSS process in the group is the
// child compiler — which usually wins the OOM lottery — but the
// scoring is non-deterministic and there's no guarantee it doesn't
// pick metis itself, taking the user's whole session down with it.
//
// We bias the bash subprocess toward +1000 (the maximum), which makes
// it always-pickable in OOM scenarios. metis itself stays at the
// default (~0), so even if the child only briefly spikes, the kernel
// will pick the child over us.
//
// Mirrors openclaw's Electron renderer subprocess hardening — same
// pattern (sh -c 'echo 1000 > /proc/self/oom_score_adj; exec ...').
// We use sh rather than a Go syscall (write_to_proc) because the
// adjustment must happen in the CHILD's address space after fork()
// but before exec() — Go's runtime doesn't expose that pre-exec
// hook portably, but the shell trick works on every Linux distro.
//
// The redirect is wrapped in `2>/dev/null` because oom_score_adj
// can be denied (PR_SET_DUMPABLE=0, SELinux, container with
// /proc remounted ro) and we don't want a stray "permission denied"
// in the user's terminal output. If the write fails the bash command
// still runs — degraded protection, not a hard fail.

// OOMWrappedCommand builds an exec.Cmd that runs `cmdStr` under the
// supplied `shell` (default /bin/bash) with the bash subprocess's
// oom_score_adj bumped to 1000 (kernel kills it first under memory
// pressure). On Linux this prepends a `/bin/sh -c 'echo 1000 > ...;
// exec <shell> -c <cmdStr>'` wrapper; on other platforms (see
// oom_other.go) it returns a plain `<shell> -c <cmdStr>`.
//
// Caller is responsible for ApplyProcessGroup/Env/etc. — this helper
// only handles the OOM-score wrapping, leaving the rest of the
// command setup to bash.go.
func OOMWrappedCommand(ctx context.Context, shell, cmdStr string) *exec.Cmd {
	if shell == "" {
		shell = "/bin/bash"
	}
	// Build the wrapper. shellQuote escapes the inner shell + cmd so
	// they survive embedding in an outer single-quoted shell string.
	inner := fmt.Sprintf("echo 1000 > /proc/self/oom_score_adj 2>/dev/null; exec %s -c %s",
		shellQuoteSingle(shell), shellQuoteSingle(cmdStr))
	return exec.CommandContext(ctx, "/bin/sh", "-c", inner)
}

// shellQuoteSingle wraps s in single quotes, escaping any embedded
// single quotes via the standard `'\''` POSIX trick. Used to embed
// arbitrary user shell commands inside the OOM-wrapper sh -c string.
//
// Example: foo'bar baz → 'foo'\''bar baz'
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
