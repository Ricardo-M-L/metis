package tui

import "testing"

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
		{"(history cleared)", "info"}, // no success keyword; falls to info
		{"branched → abc123", "success"},
		{"(session synced to disk)", "success"},
		{"tagged: scratch", "success"},
		{"(allowed) — Bash", "success"},

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
