package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestREPL_KeybindingsSlash — pre-fix regression: typing /keybindings
// (or its alias /keys) into the readline REPL produced no output. The
// signal was registered in slash.Registry but the REPL's slash dispatch
// switch had no case for it, so the handler returned ("", SignalKeybindings)
// and the empty display silently dropped on the floor. The fix wires
// SignalKeybindings to renderKeybindings() in repl.go.
//
// This test exercises the readline REPL path end-to-end with a piped
// stdin, NOT the bubbletea TUI (slash_e2e_test.go covers that path
// separately and was already passing pre-fix).
func TestREPL_KeybindingsSlash(t *testing.T) {
	cases := []struct {
		input string
		desc  string
	}{
		{"/keybindings\n/quit\n", "primary name"},
		{"/keys\n/quit\n", "alias /keys"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			out := runREPLWithInput(t, tc.input)
			// The renderKeybindings table must be present:
			// title + the two pillar shortcut rows.
			mustContain(t, out, "TUI Keybindings", "renderKeybindings title")
			mustContain(t, out, "Ctrl-C", "Ctrl-C row")
			mustContain(t, out, "Tab", "Tab row")
			mustContain(t, out, "Esc", "Esc row")
		})
	}
}

// TestREPL_KeybindingsRepeats — driving the same slash twice in one
// session must produce two render blocks (no caching / dedup bug).
func TestREPL_KeybindingsRepeats(t *testing.T) {
	out := runREPLWithInput(t, "/keybindings\n/keybindings\n/quit\n")
	count := strings.Count(out, "TUI Keybindings")
	if count != 2 {
		t.Errorf("expected 2 'TUI Keybindings' renders, got %d:\n%s", count, out)
	}
}

// runREPLWithInput spins up a minimal REPL, feeds the given input on
// stdin, and returns the captured stdout (ANSI-stripped). No LLM
// provider is wired — relying on /quit to exit before any prompt
// reaches the LLM dispatch path.
func runREPLWithInput(t *testing.T, input string) string {
	t.Helper()

	var stdout bytes.Buffer
	gate := permission.New(permission.ModeAcceptEdits)
	loop := agent.NewLoop(nil /*provider*/, tools.NewRegistry(), gate, nil, "test", 5)
	sl := slash.NewRegistry()
	slash.RegisterAll(sl, &config.Config{})

	r := &REPL{
		Loop:   loop,
		Gate:   gate,
		Slash:  sl,
		Styles: NewStyles(),
		model:  "test-model",
		cmds:   BuildREPLCommands(),
		stdin:  strings.NewReader(input),
		out:    &stdout,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.Run(ctx) // exits on /quit; ignore error from cancellation if any

	return stripANSI(stdout.String())
}

func mustContain(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("missing %s (substring %q) in REPL output:\n%s", label, needle, haystack)
	}
}
