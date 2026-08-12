package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestMonitorSandboxManagerInjectionAndWrapFailure(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.Shutdown(0) })
	watches := agent.NewMonitorRegistry(1)
	t.Cleanup(watches.StopAll)
	settings := config.ToolBashSettings{Shell: "/bin/sh"}
	monitor := NewMonitorWithSandbox(pool, watches, permission.New(permission.ModeBypassPermissions), settings, manager)
	if monitor.SandboxManager() != manager {
		t.Fatal("NewMonitorWithSandbox did not retain Manager")
	}
	if NewMonitor(pool, watches, permission.New(permission.ModeBypassPermissions), settings).WithSandbox(manager).SandboxManager() != manager {
		t.Fatal("Monitor.WithSandbox did not retain Manager")
	}

	sentinel := filepath.Join(t.TempDir(), "must-not-run")
	res, err := monitor.Execute(context.Background(), map[string]any{
		"command":     "touch '" + sentinel + "'",
		"description": "verify monitor sandbox failure",
	})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "sandbox wrap failed") || !strings.Contains(res.Output, sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("got %+v, want explicit closed-manager sandbox error", res)
	}
	if len(pool.List()) != 0 {
		t.Fatal("Monitor registered a background job after sandbox Wrap failure")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("Monitor command ran after sandbox Wrap failure: stat err=%v", err)
	}
}

func TestMonitorUsesAgentContextCwdAndSandboxFilteredEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "monitor-must-not-leak")
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.Shutdown(0) })
	watches := agent.NewMonitorRegistry(1)
	t.Cleanup(watches.StopAll)
	monitor := NewMonitorWithSandbox(
		pool,
		watches,
		permission.New(permission.ModeBypassPermissions),
		config.ToolBashSettings{Shell: "/bin/sh"},
		manager,
	)

	cwd := t.TempDir()
	probe := filepath.Join(cwd, "monitor-env.txt")
	res, err := monitor.Execute(agent.WithCwd(context.Background(), cwd), map[string]any{
		"command":     `printf '%s\n%s\n%s\n' "$PWD" "${OPENAI_API_KEY-unset}" "$TMPDIR" > monitor-env.txt`,
		"description": "record monitor cwd and environment",
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute failed: err=%v result=%+v", err, res)
	}

	var body []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body, err = os.ReadFile(probe)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Monitor did not run in context cwd: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected probe output %q", body)
	}
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0] != cwd && lines[0] != resolvedCwd {
		t.Fatalf("Monitor cwd = %q, want %q", lines[0], cwd)
	}
	if lines[1] != "unset" {
		t.Fatalf("Monitor inherited OPENAI_API_KEY: %q", lines[1])
	}
	if lines[2] != manager.TempDir() {
		t.Fatalf("Monitor TMPDIR = %q, want Manager temp %q", lines[2], manager.TempDir())
	}
}

func TestMonitorProcessGuardRejectsInCanUseAndExecute(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.Shutdown(0) })
	watches := agent.NewMonitorRegistry(1)
	t.Cleanup(watches.StopAll)
	monitor := NewMonitor(pool, watches, permission.New(permission.ModeBypassPermissions), config.ToolBashSettings{Shell: "/bin/sh"})
	in := map[string]any{
		// Signal 0 is a harmless existence probe if Execute ever regresses and
		// reaches the shell, while still exercising the raw kill-command guard.
		"command":     "kill -0 $$",
		"description": "probe shell process directly",
	}

	if got, source := monitor.CanUse(context.Background(), in); got != tools.PermissionDeny || !strings.Contains(source, "BashKill(job_id)") {
		t.Fatalf("CanUse = %v (%q), want process-guard denial", got, source)
	}
	res, err := monitor.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "BashKill(job_id)") {
		t.Fatalf("Execute = %+v, want process-guard denial", res)
	}
	if len(pool.List()) != 0 {
		t.Fatal("Monitor spawned a background job after process-guard denial")
	}
}
