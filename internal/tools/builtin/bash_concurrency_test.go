package builtin

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestBashConcurrency_InputDependent locks in the per-call dispatch
// classification: read-only invocations declare Safe (so two `ls` calls
// fan out alongside Read/Grep), mutating invocations stay Exclusive.
//
// Cases mirror claude-code's safelist + the user's likely workflow
// patterns. False-positive risk is the explicit failure mode — when a
// command isn't in the safelist or has redirection, we conservatively
// land on Exclusive.
func TestBashConcurrency_InputDependent(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want tools.Concurrency
	}{
		// Plain read-only.
		{"ls", "ls -la", tools.ConcurrencySafe},
		{"cat", "cat /etc/hosts", tools.ConcurrencySafe},
		{"grep", "grep -r foo .", tools.ConcurrencySafe},
		{"ripgrep", "rg pattern", tools.ConcurrencySafe},
		{"find", "find . -name '*.go'", tools.ConcurrencySafe},
		{"echo", "echo hello", tools.ConcurrencySafe},
		{"pwd", "pwd", tools.ConcurrencySafe},
		{"date", "date +%s", tools.ConcurrencySafe},

		// Pipelines of safe commands stay safe.
		{"cat-pipe-grep", "cat README.md | grep TODO", tools.ConcurrencySafe},
		{"ls-pipe-wc", "ls -la | wc -l", tools.ConcurrencySafe},

		// git read-only subcommands.
		{"git status", "git status", tools.ConcurrencySafe},
		{"git diff", "git diff HEAD~1", tools.ConcurrencySafe},
		{"git log", "git log --oneline -5", tools.ConcurrencySafe},
		// git mutating subcommand stays exclusive.
		{"git commit", "git commit -m foo", tools.ConcurrencyExclusive},
		{"git push", "git push origin main", tools.ConcurrencyExclusive},
		{"git config", "git config user.name foo", tools.ConcurrencyExclusive},

		// go subcommands.
		{"go list", "go list ./...", tools.ConcurrencySafe},
		{"go env", "go env GOPATH", tools.ConcurrencySafe},
		{"go vet", "go vet ./...", tools.ConcurrencySafe},
		{"go build", "go build ./...", tools.ConcurrencyExclusive},
		{"go install", "go install foo", tools.ConcurrencyExclusive},

		// Mutating shell commands.
		{"rm", "rm -rf /tmp/foo", tools.ConcurrencyExclusive},
		{"mv", "mv a b", tools.ConcurrencyExclusive},
		{"cp", "cp a b", tools.ConcurrencyExclusive},
		{"mkdir", "mkdir foo", tools.ConcurrencyExclusive},
		{"npm install", "npm install", tools.ConcurrencyExclusive},

		// Redirection — even with a safe binary, > makes it Exclusive.
		{"echo redirect", "echo hi > /tmp/foo", tools.ConcurrencyExclusive},
		{"cat append", "cat foo >> bar", tools.ConcurrencyExclusive},
		{"input redirect", "wc -l < foo", tools.ConcurrencyExclusive},

		// Command substitution / sub-shells — write-side risk.
		{"backtick", "echo `pwd`", tools.ConcurrencyExclusive},
		{"dollar-paren", "echo $(date)", tools.ConcurrencyExclusive},
		{"process subst", "diff <(ls a) <(ls b)", tools.ConcurrencyExclusive},

		// Mixed pipeline — one bad apple poisons it.
		{"safe-then-rm", "ls && rm foo", tools.ConcurrencyExclusive},

		// Env-var prefix doesn't fool the classifier.
		{"env-prefix-safe", "FOO=bar ls", tools.ConcurrencySafe},
		{"env-prefix-mutate", "FOO=bar rm baz", tools.ConcurrencyExclusive},

		// Unknown binary → conservative Exclusive (fail closed).
		{"unknown", "some-tool arg", tools.ConcurrencyExclusive},

		// Empty input → Exclusive.
		{"empty", "", tools.ConcurrencyExclusive},
	}

	var b Bash
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := b.Concurrency(map[string]any{"command": tc.cmd})
			if got != tc.want {
				t.Errorf("Bash.Concurrency(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestBashConcurrency_NilInput tests robustness against a Concurrency
// call that lost its input map. The dispatcher always passes one, but
// belt-and-suspenders since the type is map[string]any.
func TestBashConcurrency_NilInput(t *testing.T) {
	var b Bash
	if got := b.Concurrency(nil); got != tools.ConcurrencyExclusive {
		t.Errorf("Bash.Concurrency(nil) = %v, want Exclusive", got)
	}
	if got := b.Concurrency(map[string]any{}); got != tools.ConcurrencyExclusive {
		t.Errorf("Bash.Concurrency({}) = %v, want Exclusive", got)
	}
}
