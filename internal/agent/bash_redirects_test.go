package agent

import (
	"strings"
	"testing"
)

func TestBashRedirect_FiresOnCommonAntipatterns(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		wantSub string // substring that must appear in the hint
	}{
		{"cat single file", "cat /tmp/foo.go", "Read"},
		{"head single file", "head /tmp/log.txt", "Read"},
		{"tail single file", "tail /tmp/log.txt", "Read"},
		{"find by name", "find . -name '*.go'", "Glob"},
		{"find by iname", "find /tmp -iname x", "Glob"},
		{"grep recursive short", "grep -r foo /src", "Grep"},
		{"grep recursive long", "grep -R bar baz", "Grep"},
		{"rg recursive", "rg -r pat src", "Grep"},
		{"sed in-place", "sed -i 's/x/y/' foo.go", "Edit"},
		{"awk in-place", "awk -i inplace '{print}' foo.txt", "Edit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bashRedirect(c.cmd)
			if got == "" {
				t.Errorf("expected redirect hint for %q; got empty", c.cmd)
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("hint for %q should mention %q; got:\n%s", c.cmd, c.wantSub, got)
			}
		})
	}
}

func TestBashRedirect_QuietOnLegitShellUse(t *testing.T) {
	quiet := []string{
		"",
		"git status",
		"go test ./...",
		"npm install",
		"cat foo.txt | wc -l",    // pipe → real shell needed
		"cat a b c",              // concat, can't use Read
		"find . -type d",         // not a name pattern
		"grep foo",               // local file grep, not -r
		"echo hello",             // explicit shell intent
		"sed 's/x/y/' foo > bar", // streaming, not in-place
		"cat foo; echo done",     // compound
		"cat <<EOF\nx\nEOF",      // heredoc
		"ls && cat foo",          // chained
	}
	for _, cmd := range quiet {
		t.Run(cmd, func(t *testing.T) {
			if hint := bashRedirect(cmd); hint != "" {
				t.Errorf("legit command %q should not trigger nudge; got:\n%s", cmd, hint)
			}
		})
	}
}

func TestWrapAsSystemReminder_FormatsCorrectly(t *testing.T) {
	if got := wrapAsSystemReminder(""); got != "" {
		t.Errorf("empty hint should pass through; got %q", got)
	}
	got := wrapAsSystemReminder("use Read")
	if !strings.Contains(got, "<system-reminder>") {
		t.Errorf("wrapped output missing opening tag; got %q", got)
	}
	if !strings.Contains(got, "</system-reminder>") {
		t.Errorf("wrapped output missing closing tag; got %q", got)
	}
	if !strings.Contains(got, "use Read") {
		t.Errorf("wrapped output missing the hint body; got %q", got)
	}
}
