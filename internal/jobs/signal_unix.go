//go:build !windows

package jobs

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Tree-kill on Unix: send the signal to the negative pid (i.e. process
// group). spawn-time we set Setpgid:true so the bash child has its own
// pgid == its pid; that way `bash -c 'sleep 30 & sleep 30 & wait'`
// fans out into the same group and one kill cleans up the lot.
//
// Why explicit Setpgid instead of relying on whatever
// exec.CommandContext gives us: Go's stdlib does NOT default to
// putting the child in its own group. Without Setpgid, killing -pid
// would target our own gateway/agent group and silently SIGTERM the
// metis process. (Same trap openclaw hit in #71662.)

// ApplyProcessGroup sets up cmd so its children form a fresh, isolated
// process group rooted at cmd.Process.Pid. Idempotent — bash.go and
// jobs.Spawn both call it; we OR our flag in instead of overwriting
// any caller-supplied SysProcAttr.
//
// Exported so internal/tools/builtin/bash.go can apply it before
// Adopt runs (Spawn's own call covers the explicit run_in_background
// and direct-spawn paths).
func ApplyProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree sends signal to the entire process group. Falls back to a
// direct PID signal when negative-pid kill fails (process exited just
// now, or never had Setpgid applied — e.g. a job spawned before the
// 2026-05-09 tree-kill rollout but adopted into the new registry).
func killTree(p *os.Process, sig os.Signal) error {
	if p == nil {
		return nil
	}
	pid := p.Pid
	if pid <= 0 {
		return nil
	}
	// Negative pid → entire process group. SIGKILL/SIGTERM both work.
	if err := syscall.Kill(-pid, signalToSyscall(sig)); err == nil {
		return nil
	}
	// Group missing or permissions denied — try the leader directly so
	// at least the parent bash dies. A stray subshell may outlive but
	// that's strictly better than no kill at all.
	return p.Signal(sig)
}

// signalToSyscall maps the os.Signal we use back to syscall.Signal.
// Only SIGTERM and SIGKILL are needed — Stop's two-stage protocol uses
// these two and nothing else.
func signalToSyscall(sig os.Signal) syscall.Signal {
	switch s := sig.(type) {
	case syscall.Signal:
		return s
	}
	// Fallback: callers always pass syscall.Signal in practice.
	return syscall.SIGTERM
}

// isProcessGroupAlive returns true when at least one member of pgid is
// still around. signal 0 doesn't deliver anything; it just probes.
func isProcessGroupAlive(p *os.Process) bool {
	if p == nil || p.Pid <= 0 {
		return false
	}
	return syscall.Kill(-p.Pid, 0) == nil
}

// killTreeStaged is the two-stage protocol: SIGTERM the group, wait
// `grace` for cooperative cleanup, then SIGKILL anything still alive.
// Mirrors openclaw/src/process/kill-tree.ts:54 (graceMs default 3s,
// non-blocking via goroutine, no panic on already-dead processes).
//
// `done` is signalled when both stages finish OR the group has died on
// its own. Optional — pass nil if you don't care.
func killTreeStaged(p *os.Process, grace time.Duration, done chan<- struct{}) {
	if p == nil {
		if done != nil {
			close(done)
		}
		return
	}
	_ = killTree(p, syscall.SIGTERM)
	go func() {
		if done != nil {
			defer close(done)
		}
		if grace > 0 {
			time.Sleep(grace)
		}
		if isProcessGroupAlive(p) {
			_ = killTree(p, syscall.SIGKILL)
		}
	}()
}

// sigTerm is kept for compatibility with the existing call sites that
// just want "the polite signal". jobs.Stop's old signature used this.
func sigTerm() os.Signal {
	return syscall.SIGTERM
}
