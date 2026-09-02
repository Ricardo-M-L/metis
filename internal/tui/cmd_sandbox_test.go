package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

func sandboxTestREPL(t *testing.T, configured string) (*REPL, *sandbox.Manager) {
	t.Helper()
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: configured, TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	reg := tools.NewRegistry()
	reg.Register(bash.NewWithSandbox(
		permission.New(permission.ModeBypassPermissions),
		config.ToolBashSettings{},
		manager,
	))
	loop := agent.NewLoop(nil, reg, permission.New(permission.ModeBypassPermissions), nil, "test", 1)
	return &REPL{Loop: loop}, manager
}

func TestCmdSandbox_NoArgsShowsStatus(t *testing.T) {
	r, _ := sandboxTestREPL(t, "")
	out := cmdSandbox(r, "")
	for _, want := range []string{"sandbox", "off", "permissions", "auto-allow", "[tools.bash.sandbox]"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCmdSandboxStatusNamesWholeSubprocessBoundary(t *testing.T) {
	r, _ := sandboxTestREPL(t, "")
	out := cmdSandbox(r, "status")
	for _, want := range []string{"RunCode", "LSP", "Monitor", "MCP/Computer Use"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status omits sandboxed subprocess family %q: %s", want, out)
		}
	}
}

func TestCmdSandbox_SetsRuntimeOverride(t *testing.T) {
	for _, mode := range []string{"off", "permissions", "auto-allow"} {
		t.Run(mode, func(t *testing.T) {
			r, manager := sandboxTestREPL(t, "")
			out := cmdSandbox(r, mode)
			if mode != "off" && !manager.Available() {
				if !strings.Contains(out, "cannot enable") {
					t.Fatalf("expected unavailable-backend message; got %q", out)
				}
				if _, set := manager.RuntimeMode(); set {
					t.Fatal("rejected mode changed runtime override")
				}
				return
			}
			if !strings.Contains(out, "mode set to") {
				t.Fatalf("expected confirmation; got %q", out)
			}
			if got, set := manager.RuntimeMode(); !set || string(got) != mode {
				t.Fatalf("runtime override = %q, %v; want %q, true", got, set, mode)
			}
		})
	}
}

func TestCmdSandbox_AliasesAcceptedButCanonicalised(t *testing.T) {
	cases := map[string]string{
		"on": "permissions", "enabled": "permissions",
		"auto": "auto-allow", "autoallow": "auto-allow",
		"disabled": "off", "none": "off",
	}
	for alias, want := range cases {
		t.Run(alias, func(t *testing.T) {
			r, manager := sandboxTestREPL(t, "")
			out := cmdSandbox(r, alias)
			if want != "off" && !manager.Available() {
				if !strings.Contains(out, "cannot enable") {
					t.Fatalf("expected unavailable-backend message; got %q", out)
				}
				return
			}
			if got, set := manager.RuntimeMode(); !set || string(got) != want {
				t.Fatalf("alias %q -> %q, %v; want %q, true (output %q)", alias, got, set, want, out)
			}
		})
	}
}

func TestCmdSandbox_RejectsUnknownModeWithoutClobbering(t *testing.T) {
	r, manager := sandboxTestREPL(t, "")
	if err := manager.SetRuntimeMode("permissions"); err != nil {
		t.Fatal(err)
	}
	out := cmdSandbox(r, "permission")
	if !strings.Contains(out, "unknown mode") {
		t.Fatalf("typo should be rejected; got %q", out)
	}
	if got, set := manager.RuntimeMode(); !set || got != sandbox.ModePermissions {
		t.Fatalf("typo clobbered override: %q, %v", got, set)
	}
}

func TestCmdSandbox_StatusAndResetUsePerRuntimeManager(t *testing.T) {
	r, manager := sandboxTestREPL(t, "permissions")
	if err := manager.SetRuntimeMode("auto-allow"); err != nil {
		t.Fatal(err)
	}
	out := cmdSandbox(r, "status")
	if !strings.Contains(out, "auto-allow (runtime override; configured: permissions)") {
		t.Fatalf("status did not reflect override: %q", out)
	}
	out = cmdSandbox(r, "reset")
	if !strings.Contains(out, `effective mode is "permissions"`) {
		t.Fatalf("reset output = %q", out)
	}
	if _, set := manager.RuntimeMode(); set {
		t.Fatal("reset left runtime override active")
	}
}

func TestCmdSandboxCannotDisableBypassCredentialBoundary(t *testing.T) {
	r, manager := sandboxTestREPL(t, "permissions")
	r.Gate = permission.New(permission.ModeBypassPermissions)
	r.sandbox = manager
	if err := manager.SetRuntimeMode("auto-allow"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"off", "reset"} {
		out := cmdSandbox(r, command)
		if !strings.Contains(out, "cannot") || !strings.Contains(out, "bypassPermissions") {
			t.Fatalf("/sandbox %s output = %q", command, out)
		}
		if got, set := manager.RuntimeMode(); !set || got != sandbox.ModeAutoAllow {
			t.Fatalf("/sandbox %s changed runtime override to %q, %v", command, got, set)
		}
	}
}

func TestCmdSandbox_NoManagerIsExplicit(t *testing.T) {
	if out := cmdSandbox(nil, "status"); !strings.Contains(out, "manager is unavailable") {
		t.Fatalf("nil runtime output = %q", out)
	}
}

func TestCmdSandbox_DirectManagerSurvivesMissingBashTool(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	// A production registry may omit Bash through tools.disabled or the
	// visibility filter. The runtime boundary must remain independently wired.
	r := &REPL{sandbox: manager}
	if got := replSandboxManager(r); got != manager {
		t.Fatalf("replSandboxManager() = %p, want direct runtime manager %p", got, manager)
	}
	if out := cmdSandbox(r, "status"); !strings.Contains(out, "sandbox: off") {
		t.Fatalf("/sandbox status without Bash tool = %q", out)
	}
}

func TestSandboxStatusDisclosesLinuxUnixSocketLimit(t *testing.T) {
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode: string(sandbox.ModeOff), Network: sandbox.NetworkBlock,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	out := sandboxStatus(manager)
	if manager.Doctor().Backend == "bubblewrap" && !strings.Contains(out, "abstract AF_UNIX") {
		t.Fatalf("Linux network limitation not disclosed: %q", out)
	}
}

func TestSandboxStatusDisclosesFullAccessBypass(t *testing.T) {
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode: string(sandbox.ModePermissions), TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.SetPermissionPosture(false, true); err != nil {
		t.Fatal(err)
	}

	out := sandboxStatus(manager)
	for _, want := range []string{
		"sandbox: off",
		"fullAccess forces the process sandbox off",
		"run directly on the host",
		"unrestricted by the process sandbox",
		"coverage: bypassed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fullAccess status missing %q; got:\n%s", want, out)
		}
	}
}

func TestCmdSandboxFullAccessDoesNotClaimOverrideIsEffective(t *testing.T) {
	r, manager := sandboxTestREPL(t, "permissions")
	r.Gate = permission.New(permission.ModeFullAccess)
	r.sandbox = manager
	if err := manager.SetPermissionPosture(false, true); err != nil {
		t.Fatal(err)
	}

	out := cmdSandbox(r, "auto-allow")
	if !strings.Contains(out, "fullAccess keeps the effective process sandbox off") {
		t.Fatalf("fullAccess override response is misleading: %q", out)
	}
	if !manager.Available() {
		if _, set := manager.RuntimeMode(); set {
			t.Fatalf("unavailable backend changed the runtime override: %q", out)
		}
		if !strings.Contains(out, "mode unchanged") {
			t.Fatalf("unavailable backend did not disclose that the override was rejected: %q", out)
		}
	}
	if manager.EffectiveMode() != sandbox.ModeOff {
		t.Fatalf("fullAccess override changed effective mode to %q", manager.EffectiveMode())
	}
}
