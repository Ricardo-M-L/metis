package permission

import (
	"context"
	"testing"
)

const screenshotClaudeSkillsCommand = `ls ~/.claude/skills/ 2>/dev/null || echo "no claude skills dir"`

func TestBypassPermissions_ReadOnlySensitivePathCommandAllows(t *testing.T) {
	t.Parallel()
	g := New(ModeBypassPermissions)

	decision, source := g.Check(context.Background(), "Bash", screenshotClaudeSkillsCommand)
	if decision != DecisionAllow {
		t.Fatalf("screenshot command in bypassPermissions = %v (%s), want allow", decision, source)
	}
	if source != "mode:bypassPermissions" {
		t.Fatalf("screenshot command source = %q, want mode:bypassPermissions", source)
	}
}

func TestBypassPermissions_SensitivePathWritesStillAsk(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		cmd  string
	}{
		{name: "claude shell redirect", tool: "Bash", cmd: `echo pwned > ~/.claude/settings.json`},
		{name: "claude directory remove", tool: "Bash", cmd: `rm -rf ~/.claude`},
		{name: "metis remove", tool: "Bash", cmd: `rm ~/.metis/config.toml`},
		{name: "metis directory remove", tool: "Bash", cmd: `rm -rf ~/.metis`},
		{name: "git directory remove", tool: "Bash", cmd: `rm -rf ~/.git`},
		{name: "git config write", tool: "Bash", cmd: `git config --file ~/.git/config user.email attacker@example.com`},
		{name: "tree output write", tool: "Bash", cmd: `tree -o ~/.claude/settings.json /tmp`},
		{name: "tree attached output write", tool: "Bash", cmd: `tree -o~/.claude/settings.json /tmp`},
		{name: "file clustered compile", tool: "Bash", cmd: `file -Cvm ~/.claude/magic`},
		{name: "git external diff helper", tool: "Bash", cmd: `git diff --ext-diff ~/.git/config`},
		{name: "git textconv helper", tool: "Bash", cmd: `git show --textconv HEAD:.git/config`},
		{name: "claude direct write", tool: "Write", cmd: `/Users/x/.claude/settings.json`},
		{name: "git direct edit", tool: "Edit", cmd: `/work/project/.git/hooks/pre-commit`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := New(ModeBypassPermissions)
			decision, source := g.Check(context.Background(), tc.tool, tc.cmd)
			if decision != DecisionAsk || source != "safety_check:bypass_immune" {
				t.Fatalf("%s(%q) = %v (%s), want ask safety_check:bypass_immune", tc.tool, tc.cmd, decision, source)
			}
		})
	}
}

func TestBypassPermissions_SimilarUnprotectedDirectoryStillAllows(t *testing.T) {
	t.Parallel()
	g := New(ModeBypassPermissions)

	decision, source := g.Check(context.Background(), "Bash", `ls ~/.claude-backup`)
	if decision != DecisionAllow || source != "mode:bypassPermissions" {
		t.Fatalf("similar unprotected directory = %v (%s), want allow mode:bypassPermissions", decision, source)
	}
}

func TestSafetyPathProtectedDirectoryBoundary(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`rm -rf ~/.claude`,
		`rm -rf "/Users/x/.metis"`,
		`rm -rf /work/project/.git`,
		`cat /etc`,
	} {
		if !matchesSafetyPath(input) {
			t.Errorf("matchesSafetyPath(%q) = false, want true", input)
		}
	}
	for _, input := range []string{
		`ls ~/.claude-backup`,
		`cat /tmp/project.git/config`,
		`cat /etcetera/hosts`,
	} {
		if matchesSafetyPath(input) {
			t.Errorf("matchesSafetyPath(%q) = true, want false", input)
		}
	}
}

func TestBypassPermissions_SecretBashReadsStillAsk(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		`cat ~/.ssh/id_ed25519`,
		`cat ~/.aws/credentials 2>/dev/null || echo missing`,
	} {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			g := New(ModeBypassPermissions)
			decision, source := g.Check(context.Background(), "Bash", cmd)
			if decision != DecisionAsk || source != "secret_read:bypass_immune" {
				t.Fatalf("secret Bash read %q = %v (%s), want ask secret_read:bypass_immune", cmd, decision, source)
			}
		})
	}
}

func TestBypassPermissions_ExplicitAskAndDenyPrecedeReadOnlyExemption(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		verb   Decision
		source string
	}{
		{name: "ask", verb: DecisionAsk, source: "policy:ask-claude-skills"},
		{name: "deny", verb: DecisionDeny, source: "policy:deny-claude-skills"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := New(ModeBypassPermissions)
			g.AppendRules(Rule{
				Tool:   "Bash",
				Match:  "ls ~/.claude/skills/",
				Verb:   tc.verb,
				Source: tc.source,
			})
			decision, source := g.Check(context.Background(), "Bash", screenshotClaudeSkillsCommand)
			if decision != tc.verb || source != tc.source {
				t.Fatalf("explicit %s rule = %v (%s), want %v (%s)", tc.name, decision, source, tc.verb, tc.source)
			}
		})
	}
}

func TestBypassPermissions_ExplicitAllowCannotOverrideSensitiveWrite(t *testing.T) {
	t.Parallel()
	const command = `echo pwned > ~/.claude/settings.json`
	g := New(ModeBypassPermissions)
	g.AppendRules(Rule{Tool: "Bash", Match: command, Verb: DecisionAllow, Source: "interactive"})

	decision, source := g.Check(context.Background(), "Bash", command)
	if decision != DecisionAsk || source != "safety_check:bypass_immune" {
		t.Fatalf("explicit allow for sensitive write = %v (%s), want ask safety_check:bypass_immune", decision, source)
	}
}
