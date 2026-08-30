package bash

// bash_args_blocker.go — structured (cmd, sub-command, flag) tuple
// matcher for "this looks safe but actually escapes the sandbox".
//
// Why this is separate from applyBashPolicy:
//
//   applyBashPolicy works on full-string substring matches. That's
//   easy to express ("deny rm -rf") but brittle for cases where the
//   danger lives in a flag combination ONLY when paired with a
//   specific sub-command. `go test -exec rm` is such a case: `go`,
//   `go test`, and `-exec` are all individually fine; their
//   conjunction lets the model run arbitrary shell. Substring
//   matching either underfits ("ban -exec" — would also kill
//   `terraform -exec`) or overfits ("ban go test -exec" — bypassed
//   by `go test -exec=...`).
//
// blockedArgPatterns is the canonical list, mirroring crush's
// internal/agent/tools/bash.go::blockFuncs(). Each entry is checked
// at preflight before the command is handed to the shell so
// permission prompts never fire on these — they're flat-out refused.
//
// The subcommand and flag matchers are lenient on flag form:
// `--global` matches both `--global` and `--global=true`; the user-
// rebellion case `--gl-obal` is not matched because real package
// managers reject it too.

import (
	"errors"
	"fmt"
	"strings"
)

// blockedArgRule encodes one (cmd, subcmd-prefix, required-flags) rule.
type blockedArgRule struct {
	Cmd        string
	Sub        []string // subcommand tokens that must appear in order at the start of args
	Flags      []string // flags that must all be present somewhere
	Reason     string
	Bypassable bool // persistent install: hard-block normally, allow only in explicit bypassPermissions
}

// blockedArgRules is metis's canonical list. Add new entries with a
// short Reason — surfaced verbatim in the error so the LLM knows why.
var blockedArgRules = []blockedArgRule{
	// `go test -exec` runs an arbitrary command per test binary —
	// effectively `go test -exec "rm -rf /"`. claude-code-sourcemap +
	// crush both block.
	{Cmd: "go", Sub: []string{"test"}, Flags: []string{"-exec"}, Reason: "go test -exec runs arbitrary commands"},

	// Global package installs pollute the user's environment outside
	// the project root and persist across sessions. Block; the model
	// can use a project-local install (npm install lodash, etc.).
	{Cmd: "npm", Sub: []string{"install"}, Flags: []string{"--global"}, Reason: "global npm install pollutes user env", Bypassable: true},
	{Cmd: "npm", Sub: []string{"install"}, Flags: []string{"-g"}, Reason: "global npm install pollutes user env", Bypassable: true},
	{Cmd: "npm", Sub: []string{"i"}, Flags: []string{"--global"}, Reason: "global npm install pollutes user env", Bypassable: true},
	{Cmd: "npm", Sub: []string{"i"}, Flags: []string{"-g"}, Reason: "global npm install pollutes user env", Bypassable: true},
	{Cmd: "pnpm", Sub: []string{"add"}, Flags: []string{"--global"}, Reason: "global pnpm add pollutes user env", Bypassable: true},
	{Cmd: "pnpm", Sub: []string{"add"}, Flags: []string{"-g"}, Reason: "global pnpm add pollutes user env", Bypassable: true},
	{Cmd: "yarn", Sub: []string{"global", "add"}, Reason: "yarn global add pollutes user env", Bypassable: true},
	{Cmd: "pip", Sub: []string{"install"}, Flags: []string{"--user"}, Reason: "user-site pip install affects all projects", Bypassable: true},
	{Cmd: "pip3", Sub: []string{"install"}, Flags: []string{"--user"}, Reason: "user-site pip install affects all projects", Bypassable: true},

	// `go install` / `cargo install` / `gem install` / `brew install`
	// drop binaries onto $PATH — same persistence concern.
	{Cmd: "go", Sub: []string{"install"}, Reason: "go install drops persistent binary", Bypassable: true},
	{Cmd: "cargo", Sub: []string{"install"}, Reason: "cargo install drops persistent binary", Bypassable: true},
	{Cmd: "gem", Sub: []string{"install"}, Reason: "gem install drops persistent binary", Bypassable: true},
	{Cmd: "brew", Sub: []string{"install"}, Reason: "brew install changes user env outside project", Bypassable: true},

	// System package managers — by definition affect the whole machine.
	// Keep system package managers hard-blocked even in bypass because they
	// typically require privilege escalation and mutate the whole OS rather
	// than the user's explicitly selected workspace/toolchain.
	{Cmd: "apt", Sub: []string{"install"}, Reason: "apt install requires sudo and modifies system"},
	{Cmd: "apt-get", Sub: []string{"install"}, Reason: "apt-get install requires sudo and modifies system"},
	{Cmd: "dnf", Sub: []string{"install"}, Reason: "dnf install requires sudo and modifies system"},
	{Cmd: "yum", Sub: []string{"install"}, Reason: "yum install requires sudo and modifies system"},
	{Cmd: "apk", Sub: []string{"add"}, Reason: "apk add modifies system packages"},
	{Cmd: "pacman", Flags: []string{"-S"}, Reason: "pacman -S modifies system packages"},
	{Cmd: "zypper", Sub: []string{"install"}, Reason: "zypper install modifies system packages"},
}

// applyBashArgsBlocker returns an error when cmd matches one of the
// canonical blocked-arg patterns. Returns nil if no rule fires.
//
// The cmd here is the WHOLE command line as the user/LLM typed it.
// We tokenise once and walk every rule against the same token slice
// so the cost is O(rules × tokens) — small for a 20-rule list and
// short commands.
func applyBashArgsBlocker(cmd string) error {
	return applyBashArgsBlockerForBypass(cmd, false)
}

// applyBashArgsBlockerForBypass keeps exploit-shape rules hard in every mode
// while allowing ordinary persistent package installs only after the user has
// explicitly selected bypassPermissions. Policy deny-lists and the optional OS
// sandbox still run independently and may reject the command.
func applyBashArgsBlockerForBypass(cmd string, bypass bool) error {
	tokens := tokeniseShellCommand(cmd)
	if len(tokens) == 0 {
		return nil
	}
	for _, r := range blockedArgRules {
		if r.matches(tokens) {
			if bypass && r.Bypassable {
				continue
			}
			reason := r.Reason
			if reason == "" {
				reason = "policy"
			}
			return fmt.Errorf("blocked: %s %s — %s",
				r.Cmd, strings.Join(r.Sub, " "), reason)
		}
	}
	return nil
}

// matches checks whether `tokens` describes the rule's (cmd, sub, flags) tuple.
func (r blockedArgRule) matches(tokens []string) bool {
	if tokens[0] != r.Cmd {
		return false
	}
	rest := tokens[1:]
	args, flags := splitArgsFlags(rest)
	// Subcommand prefix must match in order at the start of the
	// non-flag positional args.
	if len(r.Sub) > 0 {
		if len(args) < len(r.Sub) {
			return false
		}
		for i, sub := range r.Sub {
			if args[i] != sub {
				return false
			}
		}
	}
	// Every required flag must be present somewhere in `flags`.
	for _, want := range r.Flags {
		if !contains(flags, want) {
			return false
		}
	}
	return true
}

// splitArgsFlags partitions tokens into positional args + flags.
// Mirrors crush's `internal/shell/shell.go::splitArgsFlags`. Flag
// values written `--global=true` collapse to `--global` so the rule
// matcher doesn't need to enumerate every value variant.
func splitArgsFlags(tokens []string) (args []string, flags []string) {
	args = make([]string, 0, len(tokens))
	flags = make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "-") {
			name := tok
			if before, _, ok := strings.Cut(tok, "="); ok {
				name = before
			}
			flags = append(flags, name)
		} else {
			args = append(args, tok)
		}
	}
	return args, flags
}

// tokeniseShellCommand splits a command line into tokens at whitespace
// boundaries. Quoted strings stay grouped (so `--exec "rm -rf /"`
// becomes `--exec` + `rm -rf /` rather than 4 tokens).
//
// Not a full shell parser — it doesn't handle escapes inside double
// quotes or here-docs — but the blocker only needs to match
// flag/subcommand structure, and we use the WHOLE command for the
// tokeniser, so parse drift between zsh's actual interpretation and
// our split costs at most a missed block (preferred to a false
// positive that breaks legit commands).
func tokeniseShellCommand(cmd string) []string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for _, r := range cmd {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// errBashArgsBlocked is the public error sentinel — callers can
// errors.Is against this if they want to react specifically (e.g. UI
// surfacing as a different colour). Currently nothing does.
var errBashArgsBlocked = errors.New("bash args blocked")
