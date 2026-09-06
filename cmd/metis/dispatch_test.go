package main

// Dispatcher tests — 2026-05-18 regression for the
// `metis -p X -m Y run --mode ask 'prompt'` arg-ordering bug.
// Pre-fix, the switch on args[0] only saw "-p", fell through to
// `default:`, and cmdRun's parseFlags then swallowed "run --mode ask"
// into the prompt string. Post-fix, findEarlySubcommand hoists the
// verb forward so dispatch routes to cmdRun(args[1:]) correctly.

import (
	"context"
	"strings"
	"testing"
)

func TestFindEarlySubcommand_RunAfterGlobals(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantIdx int
		wantFnd bool
	}{
		{"run after -p X -m Y", []string{"-p", "minimax", "-m", "MiniMax-M2.7", "run", "--mode", "ask", "hi"}, 4, true},
		{"run at args[0]", []string{"run", "hi"}, 0, false}, // already at 0, no hoist needed (switch handles it)
		{"chat after -p X", []string{"-p", "minimax", "chat"}, 2, true},
		{"sessions after -d", []string{"-d", "sessions", "list"}, 1, true},
		{"no subcommand (inline prompt)", []string{"-p", "minimax", "explain this code"}, 0, false},
		{"empty", []string{}, 0, false},
		{"only flags", []string{"-p", "minimax", "-m", "X"}, 0, false},
		{"login is model value", []string{"--model", "login", "run", "hi"}, 2, true},
		{"login is provider value", []string{"--provider", "login", "run", "hi"}, 2, true},
		{"logout after provider", []string{"--provider", "openai", "logout"}, 2, true},
		{"verb-looking model value without command", []string{"--model", "login"}, 0, false},
		{"run as prompt fragment after another positional",
			[]string{"-p", "minimax", "go", "run", "the", "tests"}, 0, false}, // "go" is positional, breaks the all-flag chain
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			idx, found := findEarlySubcommand(c.args, 16)
			if found != c.wantFnd {
				t.Errorf("found=%v want=%v (args=%v)", found, c.wantFnd, c.args)
			}
			if found && idx != c.wantIdx {
				t.Errorf("idx=%d want=%d (args=%v)", idx, c.wantIdx, c.args)
			}
		})
	}
}

func TestDispatchLeadingProviderLegacyAuthLogin(t *testing.T) {
	// --help keeps this a non-interactive routing test. Before the fix dispatch
	// hoisted the arguments to `auth --provider openai login --help`, and
	// cmdAuth rejected --provider as an unknown subcommand.
	err := dispatch(context.Background(), []string{"--provider", "openai", "auth", "login", "--help"})
	if err != nil {
		t.Fatalf("leading provider auth login dispatch: %v", err)
	}
}

func TestDispatchLeadingProviderLegacyAuthLogout(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	err := dispatch(context.Background(), []string{"--provider", "openai", "auth", "logout"})
	if err != nil {
		t.Fatalf("leading provider auth logout dispatch: %v", err)
	}
}

func TestDispatchLeadingUnsupportedGlobalStillFailsClearlyForLegacyAuthLogin(t *testing.T) {
	err := dispatch(context.Background(), []string{"--model", "gpt-test", "auth", "login", "--help"})
	if err == nil || !strings.Contains(err.Error(), "not applicable") {
		t.Fatalf("leading model auth login error = %v, want not-applicable error", err)
	}
}
