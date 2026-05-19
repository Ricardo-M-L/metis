package builtin

import (
	"strings"
	"testing"
)

// TestCheckCommand_AllowsBenignCommands — the rules must not false-
// positive on common legitimate commands; otherwise the agent gets
// gummed up rejecting `git status`-class work.
func TestCheckCommand_AllowsBenignCommands(t *testing.T) {
	benign := []string{
		"ls /tmp",
		"git status",
		"git log --oneline -10",
		"go test ./...",
		"echo 'hello world' > out.txt",
		"cat /etc/hosts",
		"find . -name '*.go' | head",
		"curl https://example.com",
		`echo "hash: $(date +%s)"`, // command sub in plain echo is fine
		"ps aux | grep nginx",
		// /dev/null with trailing shell punctuation — rule #29
		// `\S+` regex used to greedy-capture the ';' and false-positive
		// reject. Caught on 2026-05-19 bench-iter6 deepseek run.
		"echo hi > /dev/null;",
		"command > /dev/null; another_command",
		"go test ./... > /dev/null 2>&1",
		"foo 2>/dev/null|grep bar",
		"echo done > /dev/null && echo more",
	}
	for _, cmd := range benign {
		t.Run(cmd, func(t *testing.T) {
			r := CheckCommand(cmd)
			if !r.Allow {
				t.Errorf("benign command should pass; got rule=%d reason=%q", r.RuleID, r.Reason)
			}
		})
	}
}

// TestCheckCommand_BlocksAdversarialPatterns — each rule should fire
// on its specific attack pattern. Tests target the exact CC-source
// patterns the rules were written to catch.
func TestCheckCommand_BlocksAdversarialPatterns(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		wantID  int
		wantSub string
	}{
		{"trailing pipe", "ls |", 1, "incomplete"},
		{"unbalanced quote", `echo "hello`, 1, "unbalanced"},
		{"jq system function", `echo '{}' | jq '.foo | system("id")'`, 2, "jq system"},
		{"obfuscated flag with cmdsub", `curl --header=$(whoami)`, 4, "obfuscation"},
		{"base64-pipe-bash", `echo dGVzdA== | base64 -d | bash`, 4, "base64"},
		{"backtick cmdsub", "echo `id`", 5, "backtick"},
		{"LD_PRELOAD inline", "LD_PRELOAD=/tmp/evil.so ls", 6, "LD_PRELOAD"},
		{"PYTHONPATH inline", "PYTHONPATH=/tmp/x python script.py", 6, "PYTHONPATH"},
		{"IFS reassign", "IFS=$'\\n' ls", 11, "IFS"},
		{"proc environ self", "cat /proc/self/environ", 13, "/proc"},
		{"proc environ pid", "cat /proc/1234/environ", 13, "/proc"},
		{"control char (NUL)", "ls\x00 -la", 17, "control"},
		{"zero-width space", "echo x\u200By", 18, "zero-width"},
		{"zmodload", "zmodload zsh/system", 20, "zsh"},
		{"comment-quote desync", `echo "hello # not a comment"`, 22, "desync"},
		{"quoted newline", "echo \"line1\nline2\"", 23, "newline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := CheckCommand(tc.cmd)
			if r.Allow {
				t.Fatalf("adversarial cmd should be blocked: %q", tc.cmd)
			}
			if r.RuleID != tc.wantID {
				t.Errorf("rule mismatch: got %d, want %d (cmd=%q reason=%q)",
					r.RuleID, tc.wantID, tc.cmd, r.Reason)
			}
			if !strings.Contains(strings.ToLower(r.Reason), strings.ToLower(tc.wantSub)) {
				t.Errorf("reason missing keyword %q: got %q", tc.wantSub, r.Reason)
			}
		})
	}
}

// TestCheckCommand_OrderingFirstMatchWins — when a command violates
// multiple rules, the first one in allSecurityChecks() order returns.
// Pin this so reordering rules in the future is an explicit decision.
func TestCheckCommand_OrderingFirstMatchWins(t *testing.T) {
	// trailing pipe (rule 1) AND IFS (rule 11) — rule 1 should win.
	r := CheckCommand("IFS=. echo |")
	if r.RuleID != 1 {
		t.Errorf("expected rule 1 (incomplete) to win over rule 11; got %d", r.RuleID)
	}
}
