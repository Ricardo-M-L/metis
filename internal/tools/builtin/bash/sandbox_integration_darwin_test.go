//go:build darwin

package bash

import (
	"context"
	"os"
	"os/exec"
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

func TestBashSandboxPermissionsBypassStillIsolatesForegroundAndBackground(t *testing.T) {
	if !sandbox.Doctor().Available {
		t.Skipf("sandbox backend unavailable: %v", sandbox.Doctor().Err)
	}
	cwd := t.TempDir()
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	preflightBashSandbox(t, manager, cwd)

	gate := permission.New(permission.ModeBypassPermissions)
	tool := NewWithSandbox(gate, config.ToolBashSettings{Shell: "/bin/sh", TimeoutSeconds: 5}, manager)
	ctx := agent.WithCwd(context.Background(), cwd)

	allowed := filepath.Join(cwd, "allowed.txt")
	res, err := tool.Execute(ctx, map[string]any{
		"command":     "printf allowed > " + shellQuoteForTest(allowed),
		"description": "write inside sandbox cwd",
	})
	if err != nil || res.IsError {
		t.Fatalf("cwd write failed: err=%v result=%+v", err, res)
	}
	if data, err := os.ReadFile(allowed); err != nil || string(data) != "allowed" {
		t.Fatalf("cwd write data=%q err=%v", data, err)
	}

	tempResult, err := tool.Execute(ctx, map[string]any{
		"command":     `printf %s "$TMPDIR"`,
		"description": "inspect sandbox temporary directory",
	})
	if err != nil || tempResult.IsError || tempResult.Output != manager.TempDir() {
		t.Fatalf("TMPDIR result=%+v err=%v, want %q", tempResult, err, manager.TempDir())
	}

	outsideDir := t.TempDir()
	foregroundOutside := filepath.Join(outsideDir, "foreground-denied")
	foregroundCommand := "printf denied > " + shellQuoteForTest(foregroundOutside)
	perm, source := tool.CanUse(ctx, map[string]any{"command": foregroundCommand})
	if perm != tools.PermissionAllow || source != "mode:bypassPermissions" {
		t.Fatalf("bypass gate = %v (%s), want allow; sandbox remains independent", perm, source)
	}
	res, err = tool.Execute(ctx, map[string]any{
		"command":     foregroundCommand,
		"description": "verify foreground write isolation",
	})
	if err != nil || !res.IsError {
		t.Fatalf("outside foreground write should fail: err=%v result=%+v", err, res)
	}
	if _, err := os.Stat(foregroundOutside); !os.IsNotExist(err) {
		t.Fatalf("sandbox allowed foreground outside write: stat err=%v", err)
	}

	pool := jobs.NewRegistry(t.TempDir())
	tool.Jobs = pool
	t.Cleanup(func() { pool.Shutdown(100 * time.Millisecond) })
	backgroundOutside := filepath.Join(outsideDir, "background-denied")
	res, err = tool.Execute(ctx, map[string]any{
		"command":           "printf denied > " + shellQuoteForTest(backgroundOutside),
		"description":       "verify background write isolation",
		"run_in_background": true,
	})
	if err != nil || res.IsError {
		t.Fatalf("background spawn failed before command ran: err=%v result=%+v", err, res)
	}
	select {
	case notification := <-pool.Notify():
		if notification.Status != jobs.StatusFailed {
			t.Fatalf("sandbox-denied background status=%s, want failed", notification.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sandboxed background command")
	}
	if _, err := os.Stat(backgroundOutside); !os.IsNotExist(err) {
		t.Fatalf("sandbox allowed background outside write: stat err=%v", err)
	}
}

func preflightBashSandbox(t *testing.T, manager *sandbox.Manager, cwd string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.Dir = cwd
	wrapped, err := manager.Wrap(cmd, sandbox.Request{Cwd: cwd})
	if err != nil {
		t.Skipf("sandbox wrap unavailable: %v", err)
	}
	output, err := wrapped.CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
		t.Skip("host exposes sandbox-exec but forbids nested sandbox application")
	}
	t.Fatalf("sandbox preflight failed: %v: %s", err, output)
}
