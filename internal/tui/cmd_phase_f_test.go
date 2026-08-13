package tui

// Phase-F slash smoke tests. Pure-string handlers only; the keybind
// for Ctrl+X mode flip is exercised in the keybind tests file once
// the submit-side dispatch wires up.

import (
	"strings"
	"testing"
)

func TestThoughts_NoSession(t *testing.T) {
	r := &REPL{}
	out := cmdThoughts(r, "")
	if !strings.Contains(out, "no active session") {
		t.Errorf("expected 'no active session'; got: %q", out)
	}
}

func TestUltraplan_DefaultTarget(t *testing.T) {
	r := &REPL{}
	out := cmdUltraplan(r, "")
	if !strings.Contains(out, "ultra-detailed plan") {
		t.Errorf("expected ultra-plan frame; got: %q", out)
	}
	if !strings.Contains(out, "task I just described") {
		t.Errorf("default target should appear; got: %q", out)
	}
}

func TestUltraplan_ExplicitTarget(t *testing.T) {
	r := &REPL{}
	out := cmdUltraplan(r, "the auth refactor")
	if !strings.Contains(out, "the auth refactor") {
		t.Errorf("explicit target should appear; got: %q", out)
	}
}

func TestOnboarding_RendersSetupSteps(t *testing.T) {
	r := &REPL{}
	out := cmdOnboarding(r, "")
	for _, want := range []string{
		"metis auth login",
		"/init",
		"config.toml",
		"~/.metis/skills",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("onboarding missing %q in:\n%s", want, out)
		}
	}
}
