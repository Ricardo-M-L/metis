//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestApplyWorkspaceHookPolicyExecutesUserButNotUntrustedProjectHooks(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)

	userMarker := filepath.Join(t.TempDir(), "user-hook-ran")
	projectMarker := filepath.Join(t.TempDir(), "project-hook-ran")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
[[hooks.session_start]]
type = "command"
command = 'printf user > %q'
`, userMarker)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(fmt.Sprintf(`
[[hooks.session_end]]
type = "command"
command = 'printf project > %q'
`, projectMarker)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyWorkspaceHookPolicy(cfg, false); err != nil {
		t.Fatal(err)
	}
	reg := pubhook.NewRegistry()
	rtpkg.LoadConfigHooks(reg, &cfg.Hooks)
	reg.EmitSessionStart(context.Background(), pubhook.Context{}, "system", "model")
	reg.EmitSessionEnd(context.Background(), pubhook.Context{}, 1, "done")
	if _, err := os.Stat(userMarker); err != nil {
		t.Fatalf("trusted user hook did not run: %v", err)
	}
	if _, err := os.Stat(projectMarker); !os.IsNotExist(err) {
		t.Fatalf("untrusted project hook executed, stat err=%v", err)
	}
}

func TestApplyWorkspaceHookPolicyAllowsProjectHooksAfterPersistedTrust(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)

	projectMarker := filepath.Join(t.TempDir(), "project-hook-ran")
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(fmt.Sprintf(`
[[hooks.session_start]]
type = "command"
command = 'printf project > %q'
`, projectMarker)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := addTrustedDir(project); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyWorkspaceHookPolicy(cfg, false); err != nil {
		t.Fatal(err)
	}
	reg := pubhook.NewRegistry()
	rtpkg.LoadConfigHooks(reg, &cfg.Hooks)
	reg.EmitSessionStart(context.Background(), pubhook.Context{}, "system", "model")
	if _, err := os.Stat(projectMarker); err != nil {
		t.Fatalf("trusted project hook did not run: %v", err)
	}
}

func TestApplyWorkspaceHookPolicyBareDisablesAllCommandHooks(t *testing.T) {
	cfg := &config.Config{Hooks: config.HooksConfig{
		SessionStart: []config.HookSpec{{Command: "must-not-run"}},
	}}
	if err := applyWorkspaceHookPolicy(cfg, true); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.SessionStart) != 0 {
		t.Fatalf("bare startup retained executable hooks: %+v", cfg.Hooks)
	}
}

func TestSetupRuntimeHeadlessDoesNotRegisterUntrustedProjectHook(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("METIS_AUTO_MEMORY", "0")
	t.Chdir(project)

	userMarker := filepath.Join(t.TempDir(), "user-runtime-hook-ran")
	projectMarker := filepath.Join(t.TempDir(), "project-runtime-hook-ran")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
[[hooks.session_start]]
type = "command"
command = 'printf user > %q'
`, userMarker)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(fmt.Sprintf(`
[[hooks.session_end]]
type = "command"
command = 'printf project > %q'
`, projectMarker)), 0o600); err != nil {
		t.Fatal(err)
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{noAuthWizard: true})
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()
	rt.loop.Hooks.EmitSessionStart(context.Background(), pubhook.Context{}, "system", "model")
	rt.loop.Hooks.EmitSessionEnd(context.Background(), pubhook.Context{}, 1, "done")
	if _, err := os.Stat(userMarker); err != nil {
		t.Fatalf("headless runtime lost trusted user hook: %v", err)
	}
	if _, err := os.Stat(projectMarker); !os.IsNotExist(err) {
		t.Fatalf("headless runtime registered untrusted project hook, stat err=%v", err)
	}
}
