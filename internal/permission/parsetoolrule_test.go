package permission

import "testing"

func TestParseToolRule(t *testing.T) {
	cases := []struct {
		in       string
		wantTool string
		wantCont string
	}{
		{"Bash(git pull:*)", "Bash", "git pull:*"},
		{"Edit(/etc/**)", "Edit", "/etc/**"},
		{"Write", "Write", ""},
		{"*", "*", ""},
		{"  Bash(echo:*)  ", "Bash", "echo:*"},
		{" Write ", "Write", ""},
		// Unterminated paren fails safe to a bare (typo'd) tool name, not a
		// wildcard — content stays empty only because there's no closing ).
		{"Bash(foo", "Bash(foo", ""},
		{"Bash()", "Bash", ""},
	}
	for _, c := range cases {
		gotTool, gotCont := ParseToolRule(c.in)
		if gotTool != c.wantTool || gotCont != c.wantCont {
			t.Errorf("ParseToolRule(%q) = (%q,%q), want (%q,%q)",
				c.in, gotTool, gotCont, c.wantTool, c.wantCont)
		}
	}
}

// A parsed rule must round-trip through MatchesRuleContent the way the
// cron evaluator relies on: prefix rules honor the command-chain guard.
func TestParseToolRuleMatchesContent(t *testing.T) {
	_, content := ParseToolRule("Bash(echo:*)")
	if !MatchesRuleContent(content, "echo hello") {
		t.Errorf("echo:* should match 'echo hello'")
	}
	if MatchesRuleContent(content, "echo hi; rm -rf /") {
		t.Errorf("echo:* must NOT match a chained 'echo hi; rm -rf /'")
	}
	_, bare := ParseToolRule("Write")
	if !MatchesRuleContent(bare, "/any/path") {
		t.Errorf("bare tool rule (empty content) should match any input")
	}
}
