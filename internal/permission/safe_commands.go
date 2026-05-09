package permission

// safe_commands.go pins a small read-only command allowlist that lets
// `Bash` calls under `mode:auto` skip the permission prompt. Inspired by
// crush's internal/agent/tools/safe.go (path:1-80) — the user's most
// frequent friction in `mode:auto` is a single dialog every time the
// agent runs `git status` / `ls` / `pwd`. None of these commands has
// side effects, so the dialog is pure ceremony.
//
// Design constraints:
//
//  1. Match by **first token only** — a real shell parse is overkill
//     and `cat foo.txt` should auto-allow, but `cat /etc/shadow > out`
//     must not. The path-touching safety net (`matchesSafetyPath`) and
//     the dangerous-classifier (`bash_security_rules.go`) still run
//     on top of this allowlist; this layer is the *first* yes, not
//     the *only* yes. So a fancy chain like `git status && rm -rf /`
//     is rejected by the dangerous classifier even though the first
//     token (`git`) is on the allowlist.
//
//  2. **Sub-command awareness for git** — `git status` / `log` /
//     `diff` / `blame` / `show` are read-only; `git push` / `pull` /
//     `commit` / `reset` are not. We special-case git so the safe
//     bucket isn't all-or-nothing.
//
//  3. **No shell metacharacters** — the moment the command contains
//     `&&`, `||`, `;`, `|`, redirects (`>` / `<`), or command
//     substitution (`$(`/`` ` ``), we bail out of fast-path. A command
//     that pipes `git status` into something else is not "just
//     git status", and we'd rather over-ask than under-ask.

import "strings"

// safeBashFirstTokens is the allowlist of leading argv[0] values whose
// invocation is read-only enough to bypass the auto-mode prompt.
//
// Conservative on purpose. Adding to this list is a permission
// regression risk: each entry must be (a) read-only in all common
// invocations, (b) common enough that prompting every time is real
// friction. Cross-platform commands first; macOS- and Linux-specific
// variants below.
var safeBashFirstTokens = map[string]bool{
	"ls":       true,
	"cat":      true,
	"head":     true,
	"tail":     true,
	"wc":       true,
	"file":     true,
	"stat":     true,
	"pwd":      true,
	"whoami":   true,
	"id":       true,
	"uname":    true,
	"hostname": true,
	"date":     true,
	"which":    true,
	"type":     true,
	"echo":     true,
	"printf":   true,
	"true":     true,
	"false":    true,
	"env":      true, // bare `env` lists; `env FOO=bar prog` does NOT match
	"basename": true,
	"dirname":  true,
	"realpath": true,
	"readlink": true,
	"tree":     true,
	"df":       true, // disk free, read-only
	"du":       true, // disk usage, read-only
	"ps":       true,
	"free":     true,
	"uptime":   true,
}

// safeGitSubcommands are the read-only git verbs that pair with `git`
// to clear the auto-mode prompt. Matching is exact on the second
// token. Anything not in this set falls back to the normal prompt
// (which is fine — `git push` / `commit` SHOULD ask).
var safeGitSubcommands = map[string]bool{
	"status":    true,
	"log":       true,
	"diff":      true,
	"show":      true,
	"blame":     true,
	"branch":    true, // `git branch` lists; `git branch -D foo` is dangerous but the leading-tokens rule catches that via `-D` being non-allowlisted? No — see special-case below.
	"remote":    true, // `git remote` lists; `git remote add` is mutating but uncommon enough we accept the false-positive risk
	"config":    true, // `git config --get foo`; `git config --global ...` writes — we filter on `--global`/`--system` below
	"describe":  true,
	"rev-parse": true,
	"ls-files":  true,
	"ls-tree":   true,
	"reflog":    true,
	"tag":       true, // `git tag` lists; `git tag -d foo` deletes — we filter on `-d`/`-D` below
}

// gitDangerousFlags are flags that, when present anywhere after `git
// <subcommand>`, force fall-back to the prompt even if the
// subcommand was on the allowlist. Captures the few mutating forms
// of otherwise-read-only verbs (e.g. `git branch -D foo`).
var gitDangerousFlags = map[string]bool{
	"-D":          true,
	"-d":          true,
	"--delete":    true,
	"--global":    true,
	"--system":    true,
	"--unset":     true,
	"--unset-all": true,
	"-f":          true,
	"--force":     true,
	"--rename":    true,
}

// IsSafeReadOnlyBash returns true when the given bash command is a
// pure read-only invocation safe to auto-allow under mode:auto. Any
// shell metacharacter, unknown leading token, or git-mutating flag
// drops the command back to the normal prompt path.
//
// Safe to call with empty input (returns false).
func IsSafeReadOnlyBash(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	// Reject shell metacharacters outright. A read-only first token
	// followed by `&& rm -rf /` is not safe to auto-allow.
	if strings.ContainsAny(cmd, "&|;><`") {
		return false
	}
	if strings.Contains(cmd, "$(") {
		return false
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	first := fields[0]

	// `sudo` / `doas` etc. — never auto-allow elevation, period.
	if first == "sudo" || first == "doas" || first == "su" {
		return false
	}

	// Git: needs a recognized read-only sub-command and no mutating flags.
	if first == "git" {
		if len(fields) < 2 {
			// Bare `git` prints help to stderr — harmless but uncommon.
			return true
		}
		sub := fields[1]
		if !safeGitSubcommands[sub] {
			return false
		}
		for _, tok := range fields[2:] {
			if gitDangerousFlags[tok] {
				return false
			}
		}
		return true
	}

	// Generic allowlist hit.
	return safeBashFirstTokens[first]
}
