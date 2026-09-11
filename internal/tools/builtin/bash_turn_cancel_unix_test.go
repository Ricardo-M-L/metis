//go:build !windows

package builtin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	bashbuiltin "github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

// Exercise the root Run -> dispatcher -> real Bash boundary. Direct Execute
// tests miss dispatcher cancellation detachment, which caused Esc to leave
// the TUI waiting for a command that could not observe its cancelled turn.
func TestRootTurnCancelReapsForegroundBashProcessTree(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "0")
	leaderPath := filepath.Join(tmp, "leader.pid")
	childPath := filepath.Join(tmp, "child.pid")
	command := fmt.Sprintf(`printf '%%s' "$$" > %q; tail -f /dev/null & child=$!; printf '%%s' "$child" > %q; wait`, leaderPath, childPath)
	gate := permission.New(permission.ModeFullAccess)
	registry := tools.NewRegistry()
	registry.Register(bashbuiltin.New(gate, config.ToolBashSettings{Shell: "/bin/bash", TimeoutSeconds: 600}))
	loop := agent.NewLoop(&hardStopBashProvider{command: command}, registry, gate, nil, "test", 3)
	loop.AppendUser("run the local fixture until I cancel")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	returned := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		// Reap only the exact fixture process group, including on the red run.
		if pid := readPIDFile(leaderPath); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			t.Error("fixture Run did not return during cleanup")
		}
	})
	go func() {
		defer close(returned)
		done <- loop.Run(ctx, make(chan agent.Event, 128))
	}()
	leaderPID := waitForPIDFile(t, leaderPath, 3*time.Second)
	childPID := waitForPIDFile(t, childPath, 3*time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Run error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Esc-style turn cancellation did not stop foreground Bash")
	}
	waitForProcessExit(t, leaderPID, time.Second)
	waitForProcessExit(t, childPID, time.Second)
}

func TestBashHonorsOrdinaryTurnCancellation(t *testing.T) {
	if got := tools.GetInterruptBehavior(bashbuiltin.Bash{}); got != tools.InterruptCancel {
		t.Fatalf("Bash InterruptBehavior = %v, want InterruptCancel", got)
	}
}
