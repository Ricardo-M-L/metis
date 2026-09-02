//go:build windows

package jobs

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/Ricardo-M-L/metis/internal/security"
)

const windowsTaskkillLimit = 2 * time.Second

var taskkillCommandContext = exec.CommandContext

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

// Windows has no stable process-group identity comparable to a Unix PGID.
// The staged reset path still joins taskkill /T; passive post-leader tracking
// would require owning a Job Object from spawn time.
func isProcessTreeAlive(_ *os.Process) bool { return false }

func watchProcessTree(_ *os.Process, done chan<- struct{}) context.CancelFunc {
	_, cancel := context.WithCancel(context.Background())
	if done != nil {
		close(done)
	}
	return cancel
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
func killTreeStaged(p *os.Process, grace time.Duration, done chan<- struct{}) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	if p == nil {
		if done != nil {
			close(done)
		}
		return cancel
	}
	_ = killTree(p, nil)
	go func() {
		if done != nil {
			defer close(done)
		}
		if grace < 0 {
			grace = 0
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// Force-kill regardless of liveness — taskkill /F /T on a dead
		// PID is a harmless ERROR_NOT_FOUND.
		_ = runTaskkill([]string{"/F", "/T", "/PID", strconv.Itoa(p.Pid)})
	}()
	return cancel
}

// runTaskkill is intentionally lightweight — no stderr capture, no
// retries. taskkill is fast and idempotent for our needs.
func runTaskkill(args []string) error {
	return runTaskkillWithLimit(args, windowsTaskkillLimit)
}

// runTaskkillWithLimit gives the OS helper its own deadline. Callers such as
// RunCode cancellation invoke KillProcessGroup synchronously; without this
// bound a wedged taskkill process can wedge the whole agent turn forever.
func runTaskkillWithLimit(args []string, limit time.Duration) error {
	if limit <= 0 {
		limit = windowsTaskkillLimit
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	c := taskkillCommandContext(ctx, "taskkill", args...)
	c.Env = security.RestrictedSubprocessEnv(os.Environ())
	// If taskkill or one of its inherited handles ignores cancellation, let
	// os/exec close any pipes and finish its wait path within the same bound.
	c.WaitDelay = limit
	return c.Run()
}

// sigTerm — Windows has no SIGTERM. os.Interrupt is the closest
// analogue, kept for the legacy Stop call path.
func sigTerm() os.Signal {
	return os.Interrupt
}
