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
	"describe":  true,
	"rev-parse": true,
	"ls-files":  true,
	"ls-tree":   true,
}

// gitDangerousFlags are flags that, when present anywhere after `git
// <subcommand>`, force fall-back to the prompt even if the
// subcommand was on the allowlist. Captures the few mutating forms
// of otherwise-read-only verbs (e.g. `git branch -D foo`).
var gitDangerousFlags = map[string]bool{
	"-D":                 true,
	"-d":                 true,
	"--delete":           true,
	"--unset":            true,
	"--unset-all":        true,
	"-f":                 true,
	"--force":            true,
	"--rename":           true,
	"--edit-description": true,
	"--add":              true,
	"--replace-all":      true,
	"--rename-section":   true,
	"--remove-section":   true,
	"--edit":             true,
	"-e":                 true,
}

func hasGitWriteFlag(args []string) bool {
	for _, tok := range args {
		if gitDangerousFlags[tok] || tok == "--output" || strings.HasPrefix(tok, "--output=") {
			return true
		}
	}
	return false
}

func isSafeGitBranch(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if hasGitWriteFlag(args) {
		return false
	}
	// A positional first argument creates a branch. Listing with an
	// explicit --list/-l may take any following pattern; the remaining
	// accepted flags are display-only and take no positional value.
	if args[0] == "--list" || args[0] == "-l" {
		return true
	}
	readFlags := map[string]bool{
		"-a": true, "--all": true, "-r": true, "--remotes": true,
		"-v": true, "-vv": true, "--verbose": true,
		"--show-current": true, "--ignore-case": true,
		"--column": true, "--no-column": true,
	}
	for _, arg := range args {
		if !readFlags[arg] && !strings.HasPrefix(arg, "--format=") && !strings.HasPrefix(arg, "--sort=") {
			return false
		}
	}
	return true
}

func isSafeGitRemote(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if hasGitWriteFlag(args) {
		return false
	}
	if len(args) == 1 {
		return args[0] == "-v" || args[0] == "--verbose"
	}
	// get-url only reads configured URLs; add/set-url/rename/remove/update
	// are deliberately not accepted.
	return args[0] == "get-url"
}

func isSafeGitConfig(args []string) bool {
	if len(args) == 0 || hasGitWriteFlag(args) {
		return len(args) == 0
	}
	readOp := false
	positionals := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--get", "--get-all", "--get-regexp", "--get-urlmatch",
			"--list", "-l", "--name-only", "--show-origin", "--show-scope":
			readOp = true
		case "--file", "-f", "--type", "--default":
			// These options consume one value. They are safe only in a read
			// shape, which the positional-count/readOp rule below enforces.
			if i+1 < len(args) {
				i++
			}
		case "--global", "--system", "--local", "--worktree", "--includes", "--no-includes", "-z", "--null", "--fixed-value":
			// Scope/format modifiers do not themselves write.
		default:
			if strings.HasPrefix(arg, "--type=") || strings.HasPrefix(arg, "--format=") {
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return false
			}
			positionals++
		}
	}
	// Explicit getters may legitimately take a value pattern. Without a
	// getter, zero/one positional is help or a key lookup; two writes key=value.
	return readOp || positionals <= 1
}

func isSafeGitTag(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if hasGitWriteFlag(args) {
		return false
	}
	switch args[0] {
	case "-l", "--list", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at":
		return true
	}
	return strings.HasPrefix(args[0], "--sort=") || strings.HasPrefix(args[0], "--format=")
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

	// `env` is read-only only when bare. With further argv it is an
	// arbitrary command launcher (`env touch /tmp/x`), not an inspector.
	if first == "env" {
		return len(fields) == 1
	}

	// `date` and `hostname` have privileged mutating forms on common
	// platforms. Keep their query forms but reject setters/positionals.
	if first == "date" {
		for _, tok := range fields[1:] {
			if tok == "-s" || tok == "--set" || strings.HasPrefix(tok, "--set=") || (!strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "+")) {
				return false
			}
		}
		return true
	}
	if first == "hostname" {
		if len(fields) == 1 {
			return true
		}
		return len(fields) == 2 && map[string]bool{"-f": true, "-s": true, "-d": true, "-i": true, "-I": true}[fields[1]]
	}

	// Git: needs a recognized read-only sub-command and no mutating flags.
	if first == "git" {
		if len(fields) < 2 {
			// Bare `git` prints help to stderr — harmless but uncommon.
			return true
		}
		sub := fields[1]
		args := fields[2:]
		switch sub {
		case "branch":
			return isSafeGitBranch(args)
		case "remote":
			return isSafeGitRemote(args)
		case "config":
			return isSafeGitConfig(args)
		case "tag":
			return isSafeGitTag(args)
		case "reflog":
			return len(args) == 0 || (!hasGitWriteFlag(args) && (args[0] == "show" || args[0] == "exists"))
		}
		if !safeGitSubcommands[sub] {
			return false
		}
		return !hasGitWriteFlag(args)
	}

	// Generic allowlist hit.
	return safeBashFirstTokens[first]
}
