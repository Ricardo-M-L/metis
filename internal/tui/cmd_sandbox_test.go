package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

// resetSandboxOverride is the test-only cleanup that wipes the
// runtime override. Tests should defer this; the override is a
// package-level var in `bash` and would otherwise leak across tests.
func resetSandboxOverride(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { bash.SetRuntimeSandboxMode("") })
}

func TestCmdSandbox_NoArgsShowsStatus(t *testing.T) {
	resetSandboxOverride(t)
	out := cmdSandbox(nil, "")
	for _, want := range []string{"sandbox", "off", "permissions", "auto-allow"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCmdSandbox_SetsRuntimeOverride(t *testing.T) {
	resetSandboxOverride(t)

	for _, mode := range []string{"off", "permissions", "auto-allow"} {
		bash.SetRuntimeSandboxMode("")
		out := cmdSandbox(nil, mode)
		if mode != bash.SandboxModeOff && !bash.SandboxAvailable() {
			if !strings.Contains(out, "only `off` is supported") {
				t.Errorf("%q: expected unsupported-platform message; got %q", mode, out)
			}
			if got := bash.RuntimeSandboxMode(); got != "" {
				t.Errorf("%q: rejected mode changed runtime override to %q", mode, got)
			}
			continue
		}
		if !strings.Contains(out, "mode set to") {
			t.Errorf("%q: expected confirmation; got %q", mode, out)
		}
		if got := bash.RuntimeSandboxMode(); got != mode {
			t.Errorf("%q: runtime override = %q, want %q", mode, got, mode)
		}
	}
}

func TestCmdSandbox_AliasesAcceptedButCanonicalised(t *testing.T) {
	resetSandboxOverride(t)
	cases := map[string]string{
		"on":        "permissions",
		"enabled":   "permissions",
		"auto":      "auto-allow",
		"autoallow": "auto-allow",
		"disabled":  "off",
		"none":      "off",
	}
	for alias, want := range cases {
		bash.SetRuntimeSandboxMode("")
		out := cmdSandbox(nil, alias)
		if want != bash.SandboxModeOff && !bash.SandboxAvailable() {
			if !strings.Contains(out, "only `off` is supported") {
				t.Errorf("alias %q: expected unsupported-platform message; got %q", alias, out)
			}
			if got := bash.RuntimeSandboxMode(); got != "" {
				t.Errorf("alias %q: rejected mode changed runtime override to %q", alias, got)
			}
			continue
		}
		if !strings.Contains(out, "mode set to") {
			t.Errorf("alias %q rejected: %q", alias, out)
			continue
		}
		if got := bash.RuntimeSandboxMode(); got != want {
			t.Errorf("alias %q → runtime mode %q, want %q", alias, got, want)
		}
	}
}

func TestCmdSandbox_RejectsUnknownMode(t *testing.T) {
	resetSandboxOverride(t)
	bash.SetRuntimeSandboxMode("permissions") // pre-set so we can check non-clobbering
	out := cmdSandbox(nil, "permission")      // typo, no -s
	if !strings.Contains(out, "unknown mode") {
		t.Errorf("typo should be rejected; got %q", out)
	}
	if got := bash.RuntimeSandboxMode(); got != "permissions" {
		t.Errorf("typo clobbered existing override: now %q, want untouched 'permissions'", got)
	}
}

func TestCmdSandbox_StatusReflectsOverride(t *testing.T) {
	resetSandboxOverride(t)
	bash.SetRuntimeSandboxMode("auto-allow")
	out := cmdSandbox(nil, "status")
	if !strings.Contains(out, "auto-allow (runtime override active)") {
		t.Errorf("status didn't reflect override; got %q", out)
	}
}
