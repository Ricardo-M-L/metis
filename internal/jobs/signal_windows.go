//go:build windows

package jobs

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

// ApplyProcessGroup is a no-op on Windows. We rely on taskkill /T to
// walk the descendant tree at kill time instead of grouping at spawn
// time — Job Objects would be the analogous mechanism but pulling
// them in would mean a Windows-only spawn rewrite that's not
// justified by what metis actually runs there today.
func ApplyProcessGroup(cmd *exec.Cmd) {}

// KillProcessGroup force-kills the whole tree via taskkill /F /T.
func KillProcessGroup(p *os.Process) {
	if p == nil || p.Pid <= 0 {
		return
	}
	_ = runTaskkill([]string{"/F", "/T", "/PID", strconv.Itoa(p.Pid)})
}

// killTree dispatches to taskkill /T (single signal: best effort
// graceful, no /F). On Unix this is SIGTERM-to-pgid; the equivalent
// here is taskkill without /F which sends WM_CLOSE to the leader and
// recursive WM_CLOSE to its descendants.
func killTree(p *os.Process, _ os.Signal) error {
	if p == nil || p.Pid <= 0 {
		return nil
	}
	return runTaskkill([]string{"/T", "/PID", strconv.Itoa(p.Pid)})
}

// killTreeStaged: graceful taskkill, wait grace, then taskkill /F /T.
// Same semantics as the Unix path; we just route through the
// platform's tree-kill primitive.
func killTreeStaged(p *os.Process, grace time.Duration, done chan<- struct{}) {
	if p == nil {
		if done != nil {
			close(done)
		}
		return
	}
	_ = killTree(p, nil)
	go func() {
		if done != nil {
			defer close(done)
		}
		if grace > 0 {
			time.Sleep(grace)
		}
		// Force-kill regardless of liveness — taskkill /F /T on a dead
		// PID is a harmless ERROR_NOT_FOUND.
		_ = runTaskkill([]string{"/F", "/T", "/PID", strconv.Itoa(p.Pid)})
	}()
}

// runTaskkill is intentionally lightweight — no stderr capture, no
// retries. taskkill is fast and idempotent for our needs.
func runTaskkill(args []string) error {
	c := exec.Command("taskkill", args...)
	return c.Run()
}

// sigTerm — Windows has no SIGTERM. os.Interrupt is the closest
// analogue, kept for the legacy Stop call path.
func sigTerm() os.Signal {
	return os.Interrupt
}
