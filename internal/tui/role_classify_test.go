package tui

import (
	"strings"
	"testing"
)

// TestClassifyREPLOutput — table-driven coverage of the role heuristic
// so future contributors can add a confirmation phrase + see it
// classified the right way.
func TestClassifyREPLOutput(t *testing.T) {
	cases := []struct {
		out  string
		want string
	}{
		// success
		{"rename: session title set to test sprint", "success"},
		{"effort: high", "success"},
		{"theme: dark", "success"},
		{"model: claude-opus-4-7", "success"},
		{"quick output: on", "success"},
		{"(history cleared)", "info"}, // no success keyword; falls to info
		{"branched → abc123", "success"},
		{"(session synced to disk)", "success"},
		{"tagged: scratch", "success"},
		{"(allowed) — Bash", "success"},
		{"Conversation exported to: /tmp/session.txt", "command-result"},
		{"Conversation copied to clipboard", "command-result"},

		// warning
		{"rename: no active session store", "warning"},
		{"(title: no session store available)", "warning"},
		{"(branch: no session store)", "warning"},
		{"unknown effort: foo", "warning"},
		{"(rename: not implemented)", "warning"},

		// error
		{"error: failed to write", "error"},
		{"(error fetching pr comments)", "error"},

		// neutral info
		{"(rate limit info: depends on your API provider)", "info"},
		{"some neutral output", "info"},
		{"effort: medium — balanced", "success"}, // setting prefix wins
	}
	for _, tc := range cases {
		got := classifyREPLOutput(tc.out)
		if got != tc.want {
			t.Errorf("classifyREPLOutput(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

func TestClassifyREPLOutputDoesNotPromoteInformationalBoxByBodyText(t *testing.T) {
	out := renderInfoBox("Usage", []infoRow{
		{Key: "provider", Value: "sensenova"},
		{Key: "dashboard", Value: "(unknown — check your provider's console)"},
	})
	if got := classifyREPLOutput(out); got != "info" {
		t.Fatalf("boxed usage role = %q, want info", got)
	}
	if !strings.Contains(stripANSI(out), "unknown") {
		t.Fatal("test precondition: usage box lost unknown dashboard hint")
	}
}
