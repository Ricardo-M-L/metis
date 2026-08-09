package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

func TestPlainREPLSignalClassifierCoversEveryCurrentSignal(t *testing.T) {
	// Signal is a contiguous iota enum; ThinkingDisplay is currently last.
	// This catches a missing route even for signals that no built-in emits yet.
	for sig := slash.SignalQuit; sig <= slash.SignalThinkingDisplay; sig++ {
		if got := classifyPlainREPLSignal(sig); got == plainREPLSignalUnknown {
			t.Errorf("signal %d has no plain REPL classification", sig)
		}
	}
}

func TestPlainREPLEveryRegisteredNonNoneCommandHasBackendOrExplicitTUIOnlyRoute(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	reg := slash.NewRegistry()
	slash.RegisterAll(reg, &config.Config{})

	args := map[string]string{
		"thinking":    "show",
		"title":       "coverage title",
		"add-dir":     t.TempDir(),
		"rm-dir":      t.TempDir(),
		"btw":         "coverage question",
		"batch":       "coverage task",
		"pr_comments": "1",
		"rename":      "coverage rename",
		"tag":         "coverage-tag",
	}
	seen := 0
	for _, cmd := range reg.All() {
		cmd := cmd
		t.Run(cmd.Name, func(t *testing.T) {
			_, sig := cmd.Handler(args[cmd.Name])
			if sig == slash.SignalNone {
				return
			}
			seen++
			if class := classifyPlainREPLSignal(sig); class == plainREPLSignalUnknown {
				t.Fatalf("/%s emits signal %d with no plain REPL route", cmd.Name, sig)
			}
		})
	}
	if seen < 35 {
		t.Fatalf("coverage fixture exercised only %d non-SignalNone commands", seen)
	}
}

func TestPlainREPLSignalCommandsNeverDisappear(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cases := []struct {
		command string
		want    string
	}{
		{"/status", "Session"},
		{"/stats", "Session Stats"},
		{"/permissions", "Permissions"},
		{"/release-notes", "metis"},
		{"/context", "Context Usage"},
		{"/rewind", "nothing to rewind"},
		{"/retry", "no prior user prompt"},
		{"/reload", "reload:"},
		{"/plan", "mode set: plan"},
		{"/tag coverage", "tag: no session store"},
		{"/thinking show", "unavailable in the plain readline REPL"},
		{"/btw side question", "unavailable in the plain readline REPL"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimPrefix(strings.Fields(tc.command)[0], "/"), func(t *testing.T) {
			out := runREPLWithInput(t, tc.command+"\n/quit\n")
			if !strings.Contains(out, tc.want) {
				t.Fatalf("%s output missing %q:\n%s", tc.command, tc.want, out)
			}
			if strings.Contains(out, "recognized but has no plain REPL backend handler") ||
				strings.Contains(out, "is not implemented in the plain readline REPL") {
				t.Fatalf("%s fell through the signal dispatcher:\n%s", tc.command, out)
			}
		})
	}
}

func TestPlainREPLBatchSubmitsExpandedContract(t *testing.T) {
	registry := slash.NewRegistry()
	slash.RegisterAll(registry, &config.Config{})
	r, out := newPromptTestREPL("/batch inspect auth\n/quit\n", registry)
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}

	stdout := stripANSI(out.String())
	if strings.Contains(stdout, "unavailable in the plain readline REPL") ||
		strings.Contains(stdout, "recognized but has no plain REPL backend handler") {
		t.Fatalf("/batch did not use the readline backend:\n%s", stdout)
	}
	history := r.Loop.History()
	if len(history) == 0 || len(history[0].Content) == 0 {
		t.Fatalf("/batch never entered loop history: %+v", history)
	}
	prompt := history[0].Content[0].Text
	for _, want := range []string{"Task: inspect auth", "PHASE 1 — RESEARCH", "PHASE 2 — PLAN", "PHASE 3 — EXECUTE"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expanded /batch prompt missing %q:\n%s", want, prompt)
		}
	}
}
