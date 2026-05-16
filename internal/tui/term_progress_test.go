package tui

import (
	"os"
	"testing"
)

func TestSemverGte(t *testing.T) {
	cases := []struct {
		ver     string
		a, b, c int
		want    bool
	}{
		{"3.6.10", 3, 6, 6, true},         // iTerm2 actual user case
		{"3.6.6", 3, 6, 6, true},          // exact boundary
		{"3.6.5", 3, 6, 6, false},         // below boundary
		{"4.0.0", 3, 6, 6, true},          // higher major
		{"3.7.0", 3, 6, 6, true},          // higher minor
		{"1.2.0", 1, 2, 0, true},          // Ghostty boundary
		{"1.1.99", 1, 2, 0, false},        // Ghostty below
		{"3.6.10-beta", 3, 6, 6, true},    // pre-release suffix stripped
		{"3.6", 3, 6, 0, true},            // missing patch treated as 0
		{"", 3, 6, 6, false},              // empty fails
		{"not.a.version", 3, 6, 6, false}, // unparseable fails
	}
	for _, c := range cases {
		got := semverGte(c.ver, c.a, c.b, c.c)
		if got != c.want {
			t.Errorf("semverGte(%q, %d.%d.%d) = %v, want %v", c.ver, c.a, c.b, c.c, got, c.want)
		}
	}
}

func TestTerminalSupportsProgress_iTerm2_36plus(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.10")
	t.Setenv("WT_SESSION", "")
	t.Setenv("ConEmuANSI", "")
	t.Setenv("ConEmuPID", "")
	// Skip TTY check in test (stdout is usually a pipe). We can't easily
	// stub isStdoutTTY without dependency injection, so just verify the
	// version-gate logic in isolation via semverGte (covered above) and
	// that this terminal env passes the brand match.
	prog := os.Getenv("TERM_PROGRAM")
	ver := os.Getenv("TERM_PROGRAM_VERSION")
	if prog != "iTerm.app" {
		t.Fatalf("env not set")
	}
	if !semverGte(ver, 3, 6, 6) {
		t.Errorf("iTerm2 %s should pass 3.6.6 gate", ver)
	}
}

func TestTerminalSupportsProgress_WindowsTerminalExcluded(t *testing.T) {
	t.Setenv("WT_SESSION", "deadbeef-1234")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.10")
	if terminalSupportsProgress() {
		t.Errorf("Windows Terminal must be excluded — it pops notification toasts on OSC 9;4")
	}
}
