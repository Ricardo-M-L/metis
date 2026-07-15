package permission

import (
	"context"
	"strings"
	"testing"
)

func TestMatchesRuleContent_Prefix(t *testing.T) {
	cases := []struct {
		pattern, input string
		want           bool
	}{
		{"git push:*", "git push", true},
		{"git push:*", "git push origin main", true},
		{"git push:*", "git push --force-with-lease", true},
		{"git push:*", "git pushup", false},                  // token boundary
		{"git status:*", "git status; rm -rf /tmp/x", false}, // chain guard
		{"git status:*", "git status && rm -rf /", false},
		{"git status:*", "git status || true", false},
		{"git status:*", "git status | tee /etc/passwd", false},
		{"git status:*", "git status `whoami`", false},
		{"git status:*", "git status $(whoami)", false},
		{"git status:*", "git status -s", true},
		{"npm run:*", "npm run build", true},
		{":*", "anything", false}, // malformed — never match
	}
	for _, c := range cases {
		if got := MatchesRuleContent(c.pattern, c.input); got != c.want {
			t.Errorf("MatchesRuleContent(%q, %q) = %v, want %v", c.pattern, c.input, got, c.want)
		}
	}
}

func TestMatchesRuleContent_Glob(t *testing.T) {
	cases := []struct {
		pattern, input string
		want           bool
	}{
		{"/etc/**", "/etc/passwd", true},
		{"/etc/**", "/etc/ssh/sshd_config", true},
		{"/etc/**", "/home/etc/x", false}, // anchored
		{"/home/*/notes.md", "/home/alice/notes.md", true},
		{"/home/*/notes.md", "/home/alice/sub/notes.md", false}, // * stays in segment
		{"**/*.env", "project/sub/.env", true},                  // gitignore semantics: * may be empty
		{"**/*.go", "internal/agent/loop.go", true},
		{"*.secret", "api.secret", true},
	}
	for _, c := range cases {
		if got := MatchesRuleContent(c.pattern, c.input); got != c.want {
			t.Errorf("MatchesRuleContent(%q, %q) = %v, want %v", c.pattern, c.input, got, c.want)
		}
	}
}

// Legacy plain strings keep substring semantics — existing configs
// must not change behavior.
func TestMatchesRuleContent_LegacySubstring(t *testing.T) {
	if !MatchesRuleContent("git status", "cd /x && git status -s") {
		t.Error("legacy substring must still match anywhere")
	}
	if !MatchesRuleContent("", "anything") {
		t.Error("empty pattern matches everything")
	}
}

// Pre-glob rules persisted with literal metachars (always-allow on
// `ls *.go` stores Match="ls *.go") matched by substring; the glob
// grammar must not silently un-match them — glob OR substring.
func TestMatchesRuleContent_LegacyMetacharFallback(t *testing.T) {
	// Substring would match (input contains the literal pattern);
	// anchored glob alone would not. Fallback must keep it matching.
	if !MatchesRuleContent("*.go", "ls *.go") {
		t.Error("legacy metachar rule must keep matching via substring fallback")
	}
	// Glob semantics still work where substring wouldn't.
	if !MatchesRuleContent("/etc/**", "/etc/ssh/sshd_config") {
		t.Error("glob matching must still apply")
	}
	// And the fallback must not un-anchor globs for non-literal input.
	if MatchesRuleContent("/etc/**", "/home/etc/x") {
		t.Error("substring fallback must not match when neither glob nor literal pattern is present")
	}
}

// The glob||substring fallback re-admits ONLY inputs that contain the
// pattern verbatim — exactly the pre-glob substring behavior — never an
// arbitrary unrelated input. This is the safety property behind keeping
// the fallback (2026-06-12 review re-verification). NB: glob `*` follows
// gitignore semantics (matches any non-`/` run, INCLUDING spaces), so
// command rules should use the `prefix:*` grammar, not globs — see the
// match-grammar docs in README; this test pins fallback, not that.
func TestMatchesRuleContent_FallbackReadmitsOnlyVerbatim(t *testing.T) {
	cases := []struct {
		pattern, input string
		want           bool
		why            string
	}{
		// path globs: anchored match works as intended.
		{"/etc/**", "/etc/passwd", true, "glob match"},
		// glob no-match AND no literal pattern → input stays out (the
		// fallback does NOT invent a match).
		{"/etc/**", "/var/log/syslog", false, "neither glob nor literal"},
		{"/etc/**", "/home/u/etc-backup", false, "near-miss, no literal /etc/**"},
		// glob no-match BUT input literally contains the pattern → the
		// fallback re-admits it, reproducing pre-glob substring behavior.
		{"/etc/**", "echo /etc/** >/tmp/x", true, "literal /etc/** present"},
		// `*` stays within a path segment: a slash-bearing input that the
		// author didn't intend isn't pulled in by the glob.
		{"*.txt", "/etc/passwd", false, "unrelated path"},
		{"*.txt", "notes.txt", true, "glob match in-segment"},
	}
	for _, c := range cases {
		if got := MatchesRuleContent(c.pattern, c.input); got != c.want {
			t.Errorf("MatchesRuleContent(%q, %q) = %v, want %v (%s)", c.pattern, c.input, got, c.want, c.why)
		}
	}
}

// Two policy rules: the LATER-appended wins (last-wins within a rank
// must survive the back-to-front scan + policy short-circuit).
func TestGate_PolicyLastWins(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionAllow, Source: "policy:allow"})
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny, Source: "policy:deny"})
	if d, _ := g.Check(context.Background(), "Bash", "rm x"); d != DecisionDeny {
		t.Errorf("later policy rule must win; got %v", d)
	}

	// Reverse append order → the allow now wins.
	g2 := New(ModeAsk)
	g2.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny, Source: "policy:deny"})
	g2.AppendRules(Rule{Tool: "Bash", Verb: DecisionAllow, Source: "policy:allow"})
	if d, _ := g2.Check(context.Background(), "Bash", "rm x"); d != DecisionAllow {
		t.Errorf("later policy rule must win (reversed); got %v", d)
	}
}

// Mixed ranks: a policy rule beats a later-appended lower-rank rule,
// and the short-circuit doesn't skip it.
func TestGate_PolicyBeatsLaterLowerRank(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny, Source: "policy:deny"})
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionAllow, Source: "interactive"}) // later, lower rank
	if d, _ := g.Check(context.Background(), "Bash", "rm x"); d != DecisionDeny {
		t.Errorf("policy must outrank a later interactive rule; got %v", d)
	}
}

// Resumed sources can't smuggle top-rank authority through session
// files — anything above interactive gets demoted at the boundary.
func TestSanitizeResumedSource(t *testing.T) {
	if got := SanitizeResumedSource("policy:deny"); sourceRank(got) > rankInteractive {
		t.Errorf("forged policy source kept rank %d: %q", sourceRank(got), got)
	}
	if got := SanitizeResumedSource("cli"); sourceRank(got) > rankInteractive {
		t.Errorf("forged cli source kept rank %d: %q", sourceRank(got), got)
	}
	if got := SanitizeResumedSource("interactive"); got != "interactive" {
		t.Errorf("legit source mangled: %q", got)
	}
	if got := SanitizeResumedSource("config:allow"); got != "config:allow" {
		t.Errorf("config source mangled: %q", got)
	}
}

func TestResumedSessionSourceAlwaysHasSessionLifetime(t *testing.T) {
	for _, source := range []string{"interactive", "config:allow", "policy:deny", ""} {
		got := ResumedSessionSource(source)
		if !strings.HasPrefix(got, "session:") {
			t.Errorf("ResumedSessionSource(%q) = %q, want session prefix", source, got)
		}
		if sourceRank(got) > rankInteractive {
			t.Errorf("ResumedSessionSource(%q) retained elevated rank: %q", source, got)
		}
	}
}

// Gate integration: a prefix allow rule must not be ridden by a
// command chain, and an unmatched input falls through to ASK.
func TestGate_PrefixRuleEndToEnd(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Bash", Match: "git status:*", Verb: DecisionAllow, Source: "config:allow"})

	if d, _ := g.Check(context.Background(), "Bash", "git status -s"); d != DecisionAllow {
		t.Errorf("plain git status should be allowed, got %v", d)
	}
	if d, _ := g.Check(context.Background(), "Bash", "git status; rm -rf /tmp/x"); d != DecisionAllow {
		// fall through to mode default (ask)
		if d != DecisionAsk {
			t.Errorf("chained command should fall through to ASK, got %v", d)
		}
	} else {
		t.Error("chained command must NOT ride the prefix allow rule")
	}
}

// Authority: a policy deny beats a later interactive allow; equal
// authority keeps last-appended-wins.
func TestGate_SourceAuthority(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Bash", Match: "git push:*", Verb: DecisionDeny, Source: "policy:deny"})
	// Simulates the user clicking "always allow Bash" afterwards.
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionAllow, Source: "interactive"})

	if d, src := g.Check(context.Background(), "Bash", "git push origin main"); d != DecisionDeny {
		t.Errorf("policy deny must beat interactive allow; got %v (src=%s)", d, src)
	}
	// Non-matching input: interactive allow applies normally.
	if d, _ := g.Check(context.Background(), "Bash", "ls"); d != DecisionAllow {
		t.Errorf("interactive allow should cover non-policy input, got %v", d)
	}

	// Same-rank conflict: last appended wins (historical behavior).
	g2 := New(ModeAsk)
	g2.AppendRules(Rule{Tool: "Edit", Verb: DecisionDeny, Source: "config:deny"})
	g2.AppendRules(Rule{Tool: "Edit", Verb: DecisionAllow, Source: "config:allow"})
	if d, _ := g2.Check(context.Background(), "Edit", "x.go"); d != DecisionAllow {
		t.Errorf("same-rank: last appended must win, got %v", d)
	}
}
