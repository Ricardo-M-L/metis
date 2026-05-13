package agent

// bash_redirects.go — anti-pattern nudge for Bash commands the model
// reaches for when a dedicated metis tool would do the job better.
//
// Pattern: claude-code injects `<system-reminder>` hints when its
// safer / better tool would beat raw shell. We do the same on the
// tool_result body so the model sees the nudge in the same turn as
// the command output. No refusal — the command still runs; we just
// append a one-line hint after the output.
//
// Why post-run instead of pre-run refusal:
//   - Pre-run refusal forces a retry and burns tokens on the round
//     trip without saving any "wrong choice" cost (the command never
//     ran anyway).
//   - Post-run nudge lets the model get the answer it wanted AND
//     learn to pick the right tool next time. Subsequent turns in the
//     same session pick the better tool.
//
// The detector is conservative: false positives waste a few tokens
// of reminder noise, but false negatives (missing a clear "should
// have used Read" case) are the actual loss. So we only flag the
// crystal-clear cases — `cat /abs/path` not `cat | wc -l`.

import (
	"strings"
)

// bashRedirect returns a one-line tool-redirect hint when the given
// Bash `command` invocation matches a well-known anti-pattern, or ""
// when no nudge applies. The returned string is suitable for direct
// append to the tool_result body wrapped in <system-reminder> tags.
//
// Conservative classifier: only fires on the crystal-clear cases
// (whole-line `cat /path/to/file`, `find /path -name pattern`, etc.).
// Compound shell, pipes, and conditional chains skip — those almost
// always need real shell semantics that the dedicated tools don't
// provide.
func bashRedirect(command string) string {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ""
	}
	// Compound / piped commands need real shell — skip.
	if strings.ContainsAny(cmd, "|;&") {
		return ""
	}
	if strings.Contains(cmd, ">>") || strings.Contains(cmd, "<<") {
		return ""
	}

	// Tokenize on first space — first token is the binary.
	first := cmd
	if i := strings.IndexAny(cmd, " \t"); i > 0 {
		first = cmd[:i]
	}
	// Strip leading `./`, `/usr/bin/`, etc. — we match by basename.
	if i := strings.LastIndexByte(first, '/'); i >= 0 {
		first = first[i+1:]
	}

	switch first {
	case "cat", "head", "tail", "less", "more":
		// `cat foo bar` is a concatenation, not a read — only nudge
		// when there's exactly one positional arg (looks like a peek).
		if singleFileArg(cmd) {
			return "Heads-up: for inspecting a single file, prefer the `Read` tool over `" + first + "` — Read gives line-numbered output and tracks the file's state so a subsequent Edit/Write works."
		}
	case "find":
		// `find -name` / `find /path -name X` — Glob does the same
		// without shell escaping.
		if strings.Contains(cmd, "-name ") || strings.Contains(cmd, "-iname ") {
			return "Heads-up: for filename patterns, the `Glob` tool is faster and avoids shell-escaping headaches (e.g. `**/*.go` instead of `find . -name '*.go'`)."
		}
	case "grep", "rg", "egrep", "fgrep":
		// `grep -r pattern dir` → Grep. Skip if it's a simple
		// `<output> | grep filter` (caught above by the pipe check).
		if strings.Contains(cmd, "-r ") || strings.Contains(cmd, "-R ") {
			return "Heads-up: for codebase-wide content search, the `Grep` tool wraps ripgrep with structured output — no need to invoke `" + first + "` directly."
		}
	case "sed":
		// `sed -i s/x/y/ foo.go` is the classic in-place edit. Edit is
		// safer (requires prior Read, exact-match, audit trail).
		if strings.Contains(cmd, "-i") {
			return "Heads-up: for in-place edits, prefer the `Edit` tool — it requires a prior Read (preventing drift), uses literal find-and-replace (no regex traps), and lands in the audit trail."
		}
	case "awk":
		if strings.Contains(cmd, "-i") {
			return "Heads-up: for in-place edits, prefer the `Edit` tool over `awk -i` — safer matching and an audit trail."
		}
	}
	return ""
}

// singleFileArg returns true when the command has exactly one
// positional arg after the binary name (no flags, no multiple files).
// Used to distinguish `cat foo.go` (single peek → Read) from
// `cat foo bar baz` (concatenation, can't use Read).
func singleFileArg(cmd string) bool {
	// Split, skip the binary.
	parts := strings.Fields(cmd)
	if len(parts) < 2 {
		return false
	}
	parts = parts[1:]
	// Strip leading flags. Stop at first non-flag positional.
	positional := 0
	for _, p := range parts {
		if strings.HasPrefix(p, "-") {
			continue
		}
		positional++
	}
	return positional == 1
}

// wrapAsSystemReminder formats a hint as a <system-reminder> block.
// Pass an empty hint to get back an empty string (callers don't have
// to guard the call site).
func wrapAsSystemReminder(hint string) string {
	if hint == "" {
		return ""
	}
	return "\n\n<system-reminder>\n" + hint + "\n</system-reminder>"
}
