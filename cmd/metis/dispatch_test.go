package main

// Dispatcher tests — 2026-05-18 regression for the
// `metis -p X -m Y run --mode ask 'prompt'` arg-ordering bug.
// Pre-fix, the switch on args[0] only saw "-p", fell through to
// `default:`, and cmdRun's parseFlags then swallowed "run --mode ask"
// into the prompt string. Post-fix, findEarlySubcommand hoists the
// verb forward so dispatch routes to cmdRun(args[1:]) correctly.

import (
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

func TestLooksLikeFlagOrValue(t *testing.T) {
	args := []string{"-p", "minimax", "-m", "MiniMax-M2.7", "run"}
	cases := []struct {
		idx  int
		want bool
	}{
		{0, true}, // "-p" is a flag
		{1, true}, // "minimax" is value of -p
		{2, true}, // "-m" is a flag
		{3, true}, // "MiniMax-M2.7" is value of -m
	}
	for _, c := range cases {
		if got := looksLikeFlagOrValue(args, c.idx); got != c.want {
			t.Errorf("looksLikeFlagOrValue(%q, %d) = %v, want %v", args[c.idx], c.idx, got, c.want)
		}
	}
}
