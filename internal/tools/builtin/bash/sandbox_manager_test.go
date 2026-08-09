package bash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestBashSandboxManagerInjection(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	tool := NewWithSandbox(permission.New(permission.ModeBypassPermissions), config.ToolBashSettings{}, manager)
	if tool.SandboxManager() != manager {
		t.Fatal("NewWithSandbox did not retain the injected Manager")
	}
	if got := New(permission.New(permission.ModeBypassPermissions), config.ToolBashSettings{}).WithSandbox(manager).SandboxManager(); got != manager {
		t.Fatal("WithSandbox did not retain the injected Manager")
	}
}

func TestBashSandboxWrapFailureFailsClosedForegroundAndBackground(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	settings := config.ToolBashSettings{Shell: "/bin/sh", TimeoutSeconds: 5}
	tool := NewWithSandbox(permission.New(permission.ModeBypassPermissions), settings, manager)
	sentinel := filepath.Join(t.TempDir(), "must-not-run")
	command := "printf should-not-run > " + shellQuoteForTest(sentinel)

	res, err := tool.Execute(context.Background(), map[string]any{
		"command":     command,
		"description": "verify foreground sandbox failure",
	})
	if err != nil {
		t.Fatalf("foreground Execute returned Go error: %v", err)
	}
	assertSandboxWrapToolError(t, res.Output, res.IsError)
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("foreground command ran after Wrap failure: stat err=%v", err)
	}

	tool.Jobs = jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { tool.Jobs.Shutdown(0) })
	res, err = tool.Execute(context.Background(), map[string]any{
		"command":           command,
		"description":       "verify background sandbox failure",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("background Execute returned Go error: %v", err)
	}
	assertSandboxWrapToolError(t, res.Output, res.IsError)
	if len(tool.Jobs.List()) != 0 {
		t.Fatal("background command was registered after Wrap failure")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("background command ran after Wrap failure: stat err=%v", err)
	}
}

func TestBashSandboxEnvironmentUsesPrivateTempAndKeepsSecretFilter(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModePermissions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	tool := NewWithSandbox(permission.New(permission.ModeBypassPermissions), config.ToolBashSettings{
		Sandbox: config.SandboxBashSettings{Network: "block"},
	}, manager)

	env := tool.commandEnv([]string{
		"PATH=/bin",
		"OPENAI_API_KEY=must-not-leak",
		"TMPDIR=/host/tmp",
		"HTTP_PROXY=http://host-proxy",
	})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "OPENAI_API_KEY=") {
		t.Fatalf("secret filter was bypassed:\n%s", joined)
	}
	for _, want := range []string{
		"TMPDIR=" + manager.TempDir(),
		"TMP=" + manager.TempDir(),
		"TEMP=" + manager.TempDir(),
		"HTTP_PROXY=http://localhost:0",
	} {
		if !containsEnvironment(env, want) {
			t.Errorf("sandbox child env missing %q: %v", want, env)
		}
	}
	if got := tool.sandboxNetworkPolicy(); got != sandbox.NetworkBlock {
		t.Fatalf("network policy = %q, want block", got)
	}
}

func containsEnvironment(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func assertSandboxWrapToolError(t *testing.T, output string, isError bool) {
	t.Helper()
	if !isError || !strings.Contains(output, "sandbox wrap failed") || !strings.Contains(output, sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("got IsError=%v output=%q, want explicit closed-manager sandbox error", isError, output)
	}
}

func shellQuoteForTest(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
