// Package builtin — extra bash security rules from claude-code's
// tools/BashTool/bashSecurity.ts. The pre-existing classifier.go
// covers the high-impact destructive-command checks (rm -rf, dd to
// /dev/sda, fork bombs, etc.). This file adds the CC-specific
// adversarial-input checks: shell metachar abuse, IFS injection,
// /proc/environ exfiltration, zero-width unicode spoofing, etc.
//
// Each rule is a pure function bool/string. Bash.CanUse calls all of
// them before execution; any DENY returns "blocked" to the model with
// a reason it can rephrase around. A future YOLO classifier (Task #74)
// could downgrade DENY → "ask user" for ambiguous cases.
package builtin

import (
	"regexp"
	"strings"
	"unicode"
)

// SecurityRuleResult is what each check returns.
type SecurityRuleResult struct {
	Allow  bool   // false = block
	RuleID int    // 1..23, matches CC's bashSecurity.ts numbering
	Reason string // user-facing explanation
}

// SecurityCheck is the function signature each rule implements.
type SecurityCheck func(cmd string) SecurityRuleResult

// allSecurityChecks returns the full ordered rule list. Order matters
// only for the surfaced reason — the first DENY wins. Earlier rules
// are cheaper / catch more obvious issues.
func allSecurityChecks() []SecurityCheck {
	return []SecurityCheck{
		ruleIncompleteCommand,         // 1
		ruleJQSystemFunction,          // 2
		ruleObfuscatedFlags,           // 4
		ruleShellMetacharsInArgs,      // 5
		ruleDangerousEnvVarAssignment, // 6
		ruleIFSInjection,              // 11
		ruleProcEnvironAccess,         // 13
		ruleControlCharacters,         // 17
		ruleUnicodeZeroWidth,          // 18
		ruleZshDangerousLoad,          // 20
		ruleCommentQuoteDesync,        // 22
		ruleQuotedNewlineExfil,        // 23
	}
}

// CheckCommand runs every rule. Returns the first DENY result, or an
// allow if everything passes. Caller (Bash.CanUse) maps the result to
// tools.PermissionDeny.
func CheckCommand(cmd string) SecurityRuleResult {
	for _, check := range allSecurityChecks() {
		r := check(cmd)
		if !r.Allow {
			return r
		}
	}
	return SecurityRuleResult{Allow: true}
}

// --- individual rules ------------------------------------------------------

// 1: INCOMPLETE_COMMANDS — trailing pipe / && / || or unbalanced quote
// indicates the model is mid-thought; running risks executing whatever
// the shell guesses. Better to bounce back and let it complete.
func ruleIncompleteCommand(cmd string) SecurityRuleResult {
	t := strings.TrimSpace(cmd)
	if t == "" {
		return SecurityRuleResult{Allow: true}
	}
	bad := []string{"|", "&&", "||", "&"}
	for _, suf := range bad {
		if strings.HasSuffix(t, suf) {
			return SecurityRuleResult{RuleID: 1, Reason: "command appears incomplete (trailing " + suf + ")"}
		}
	}
	// Unbalanced quote count.
	if oddCount(t, '\'') || oddCount(t, '"') {
		return SecurityRuleResult{RuleID: 1, Reason: "command has unbalanced quotes"}
	}
	return SecurityRuleResult{Allow: true}
}

// 2: JQ_SYSTEM_FUNCTION — jq's `system(...)` filter executes arbitrary
// shell. A model output blob fed through jq is a classic exfil vector.
func ruleJQSystemFunction(cmd string) SecurityRuleResult {
	if !strings.Contains(cmd, "jq") {
		return SecurityRuleResult{Allow: true}
	}
	// Loose match — `system(` anywhere after a jq invocation. Pipes
	// inside the jq filter (e.g. `'.foo | system("id")'`) are
	// expected, so we don't restrict the in-between chars.
	re := regexp.MustCompile(`(?i)\bjq\b.*\bsystem\s*\(`)
	if re.MatchString(cmd) {
		return SecurityRuleResult{RuleID: 2, Reason: "jq system() function executes arbitrary shell — denied"}
	}
	return SecurityRuleResult{Allow: true}
}

// 4: OBFUSCATED_FLAGS — flag values that look encoded / hex / base64.
// Legit calls don't need `--header=$(echo dGVzdA== | base64 -d)`.
func ruleObfuscatedFlags(cmd string) SecurityRuleResult {
	// $(...) inside flag values is the pattern — model may try to
	// hide an exec via command substitution.
	re := regexp.MustCompile(`-{1,2}[a-zA-Z][\w-]*=\$\(`)
	if re.MatchString(cmd) {
		return SecurityRuleResult{RuleID: 4, Reason: "flag value contains command substitution — possible obfuscation"}
	}
	// Base64-decode-then-pipe-to-bash pattern.
	if regexp.MustCompile(`base64\s+(-d|--decode)[^|]*\|\s*(bash|sh|zsh|ksh)`).MatchString(cmd) {
		return SecurityRuleResult{RuleID: 4, Reason: "base64-decoded payload piped to shell — denied"}
	}
	return SecurityRuleResult{Allow: true}
}

// 5: SHELL_METACHARACTERS — backticks (legacy command sub), nested
// $(...), or excessive metachar density in what looks like an argument
// position. We allow normal pipe chains; we block obvious chains-of-
// chains hiding payloads.
func ruleShellMetacharsInArgs(cmd string) SecurityRuleResult {
	// Backticks for command substitution. Modern scripts use $(...).
	// Backticks in here-docs are fine; bare backticks in commands are
	// usually adversarial.
	if strings.Contains(cmd, "`") && !strings.Contains(cmd, "<<") {
		// Allow if backtick is ONLY inside single quotes (literal).
		if !backtickOnlyInSingleQuotes(cmd) {
			return SecurityRuleResult{RuleID: 5, Reason: "legacy backtick command substitution — use $(...) explicitly"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 6: DANGEROUS_VARIABLES — assigning to PATH, LD_PRELOAD, LD_LIBRARY_PATH,
// PYTHONPATH inline before a command is a classic privilege/code-injection.
func ruleDangerousEnvVarAssignment(cmd string) SecurityRuleResult {
	// Match `VAR=value cmd` where VAR is in the danger list.
	dangerous := []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
		"PYTHONPATH", "PERL5LIB", "RUBYOPT", "NODE_OPTIONS",
	}
	for _, v := range dangerous {
		re := regexp.MustCompile(`(^|[\s;&|])` + v + `=`)
		if re.MatchString(cmd) {
			return SecurityRuleResult{RuleID: 6, Reason: "inline assignment to " + v + " can hijack execution — denied"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 11: IFS_INJECTION — `IFS=` + a separator the script then uses to split
// untrusted input. Blocking it broadly is correct for an agent context.
func ruleIFSInjection(cmd string) SecurityRuleResult {
	if regexp.MustCompile(`(^|[\s;&|])IFS\s*=`).MatchString(cmd) {
		return SecurityRuleResult{RuleID: 11, Reason: "IFS reassignment is a known shell-injection vector — denied"}
	}
	return SecurityRuleResult{Allow: true}
}

// 13: PROC_ENVIRON_ACCESS — reading /proc/<pid>/environ leaks env vars
// (often containing API keys). No legitimate agent task needs this.
func ruleProcEnvironAccess(cmd string) SecurityRuleResult {
	// /proc/self/environ or /proc/<n>/environ.
	re := regexp.MustCompile(`/proc/(self|\d+)/environ\b`)
	if re.MatchString(cmd) {
		return SecurityRuleResult{RuleID: 13, Reason: "/proc/.../environ access leaks credentials — denied"}
	}
	return SecurityRuleResult{Allow: true}
}

// 17: CONTROL_CHARACTERS — ASCII 0x00..0x1F (except \t \n \r) hidden in
// the command. Adversarial input embeds these to spoof what a user sees
// vs what the shell actually executes.
func ruleControlCharacters(cmd string) SecurityRuleResult {
	for _, r := range cmd {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return SecurityRuleResult{RuleID: 17, Reason: "command contains control characters (possible spoofing)"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 18: UNICODE_WHITESPACE — zero-width chars (U+200B, U+200C, U+200D,
// U+FEFF) and other invisible whitespace. Used to hide payloads inside
// what visually looks like a benign command.
func ruleUnicodeZeroWidth(cmd string) SecurityRuleResult {
	for _, r := range cmd {
		switch r {
		case '\u200B', '\u200C', '\u200D', '\u200E', '\u200F', '\u2060', '\uFEFF':
			return SecurityRuleResult{RuleID: 18, Reason: "command contains zero-width unicode (visual spoofing)"}
		}
		// Other "format" or "control" category runes that aren't ASCII.
		if r > 0x7F && unicode.Is(unicode.Cf, r) {
			return SecurityRuleResult{RuleID: 18, Reason: "command contains unicode format characters"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 20: ZSH_DANGEROUS_COMMANDS — zmodload loads native shared objects
// into the shell process, equivalent to LD_PRELOAD for zsh. Always deny.
func ruleZshDangerousLoad(cmd string) SecurityRuleResult {
	dangerous := []string{"zmodload ", "autoload -X", "compaudit"}
	for _, p := range dangerous {
		if strings.Contains(cmd, p) {
			return SecurityRuleResult{RuleID: 20, Reason: "zsh-specific dangerous primitive: " + strings.TrimSpace(p)}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 22: COMMENT_QUOTE_DESYNC — `# inside an open quote isn't a comment.
// A model output that looks like `cmd "arg # safe-looking comment"`
// actually executes the "comment" as part of the string. This rule
// catches the most obvious case: a quoted region containing `#` that
// looks like it should be a comment.
func ruleCommentQuoteDesync(cmd string) SecurityRuleResult {
	// Walk char by char tracking whether we're inside single/double
	// quotes. Flag if we see ` # ` inside a quote that the user
	// likely thinks is a comment.
	inSingle, inDouble := false, false
	for i, r := range cmd {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if (inSingle || inDouble) && i > 0 && cmd[i-1] == ' ' {
				return SecurityRuleResult{RuleID: 22, Reason: "`#` inside a quoted region is NOT a comment — possible desync"}
			}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 23: QUOTED_NEWLINE — newlines inside an unclosed quote let the model
// smuggle multi-line payloads past a single-line input field. Bash
// happily executes them, but the user prompt UI typically only shows
// the first line.
func ruleQuotedNewlineExfil(cmd string) SecurityRuleResult {
	if !strings.Contains(cmd, "\n") {
		return SecurityRuleResult{Allow: true}
	}
	inSingle, inDouble := false, false
	for _, r := range cmd {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '\n':
			if inSingle || inDouble {
				return SecurityRuleResult{RuleID: 23, Reason: "newline inside an unclosed quoted region — multi-line smuggling"}
			}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// --- helpers ---------------------------------------------------------------

func oddCount(s string, c rune) bool {
	n := 0
	for _, r := range s {
		if r == c {
			n++
		}
	}
	return n%2 == 1
}

func backtickOnlyInSingleQuotes(s string) bool {
	inSingle := false
	for _, r := range s {
		if r == '\'' {
			inSingle = !inSingle
		}
		if r == '`' && !inSingle {
			return false
		}
	}
	return true
}
