package tui

import "testing"

// TestLooksLikeSlashPath — gating for slash-palette trigger. Real
// commands `/help`, `/model haiku`, `/effort high` open the palette;
// pasted paths `/Users/...`, `/var/log/...` and shell-like fragments
// `/.. ./foo` suppress it.
//
// Bug report 2026-05-09 (image #10): user pasted a path
// `/Users/ricardo/Documents/.../tools` and the palette flashed a
// red "no match for ..." line instead of treating it as plain
// input. This test pins the discrimination so the regression doesn't
// come back.
func TestLooksLikeSlashPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Real commands — palette SHOULD open.
		{"/help", false},
		{"/model", false},
		{"/model haiku", false},
		{"/effort high", false},
		{"/init", false},
		{"/", false}, // discovery — palette opens with full command list

		// Paths — palette SHOULD suppress.
		{"/Users/ricardo/Documents/foo", true},
		{"/var/log/syslog", true},
		{"/etc/passwd", true},
		{"/tmp/x.txt", true},

		// Path-ish edge cases.
		{"/.. /foo", true},               // dot-dot is not command-shape
		{"/foo.bar", true},               // dot-in-name → not command
		{"/foo with spaces /bar", false}, // head "foo" is command-shape (space splits args)
		{"/foo/bar with text", true},     // head "foo/bar" has slash → path
		{"/cmd-name", false},             // hyphen allowed
		{"/cmd_name", false},             // underscore allowed
		{"/123", false},                  // numeric allowed (unusual but parseable)

		// Non-slash inputs — fall-through (function returns false).
		{"hello", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := looksLikeSlashPath(tc.in); got != tc.want {
				t.Errorf("looksLikeSlashPath(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
