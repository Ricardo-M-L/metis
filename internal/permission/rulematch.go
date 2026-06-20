package permission

// Structured rule-content matching — claude-code parity for the
// PermissionRule ruleContent grammar (restored-src/src/types/
// permissions.ts:55-91). Three pattern forms, recognized in order:
//
//	"git push:*"   → command-prefix match. The input must start with
//	                 the prefix at a token boundary; a remainder that
//	                 introduces a NEW shell command (;, &&, ||, |,
//	                 backtick, $(, newline) never matches, so an
//	                 allow rule for `git status:*` cannot be ridden
//	                 by `git status; rm -rf /`.
//	"/etc/**"      → path glob (full-string). `**` crosses path
//	                 separators, `*` stays within one segment,
//	                 `?` is any single non-separator char.
//	"git status"   → legacy substring (backward compatible with every
//	                 existing config.toml / persistent rule).
//
// Why substring stays: thousands of existing rules in the wild use it
// and a silent semantic change would flip their decisions. New rules
// should prefer the first two forms; the gate docs + `metis config`
// guidance point there.

import (
	"regexp"
	"strings"
	"sync"
)

// ParseToolRule splits a compact `Tool(content)` rule string into its
// tool and content parts — claude-code's permission-rule surface syntax
// (`Bash(git pull:*)`, `Edit(/etc/**)`). A bare `Write` (no parens)
// parses as tool-only with empty content, which MatchesRuleContent
// treats as "any input for this tool". The content half is matched via
// MatchesRuleContent, so it accepts the same prefix/glob/substring grammar.
//
// Whitespace around the tool name and the whole string is trimmed; an
// unterminated `Bash(foo` (no closing paren) is treated as a bare tool
// name so a typo fails safe (matches nothing useful) rather than silently
// becoming a wildcard. `*` as the tool means "any tool".
func ParseToolRule(s string) (tool, content string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		return strings.TrimSpace(s[:i]), s[i+1 : len(s)-1]
	}
	return s, ""
}

// MatchesRuleContent reports whether one rule's Match pattern matches
// the stringified tool input. Empty pattern matches everything (the
// rule is tool-scoped only).
func MatchesRuleContent(pattern, input string) bool {
	if pattern == "" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, ":*"); ok {
		return matchCommandPrefix(prefix, input)
	}
	if hasGlobMeta(pattern) {
		// Glob first, then legacy-substring fallback. Rules written
		// BEFORE the glob grammar existed stored literal metachars
		// (e.g. an always-allow persisted for the command `ls *.go`
		// keeps Match="ls *.go") and matched by substring; an anchored
		// glob alone would silently stop matching them (2026-06-11
		// review finding). The fallback can only WIDEN a glob rule by
		// inputs that literally contain the pattern text — for allow
		// rules that's the pre-glob status quo, for deny rules wider
		// is the safe direction.
		return matchGlob(pattern, input) || strings.Contains(input, pattern)
	}
	return strings.Contains(input, pattern)
}

// commandChainRe spots anything that starts a NEW command after the
// matched prefix: `;`, `&&`, `||`, a pipe, command substitution, or a
// newline. Conservative on purpose — a prefix rule is "this command,
// with arguments", never "this command, then whatever else".
var commandChainRe = regexp.MustCompile("[;|&`\n]|\\$\\(")

// matchCommandPrefix implements the `prefix:*` form.
func matchCommandPrefix(prefix, input string) bool {
	if prefix == "" {
		return false // ":*" alone is a malformed rule — never match
	}
	if input == prefix {
		return true
	}
	if !strings.HasPrefix(input, prefix) {
		return false
	}
	rest := input[len(prefix):]
	// Token boundary: `git push:*` must not match `git pushup`. A
	// prefix already ending in a non-word char (e.g. "/etc/") is its
	// own boundary.
	if isWordByte(prefix[len(prefix)-1]) && isWordByte(rest[0]) {
		return false
	}
	// Chain guard: the remainder may extend THIS command's arguments,
	// not start another one.
	return !commandChainRe.MatchString(rest)
}

func isWordByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// globCache memoizes compiled glob patterns — Check runs on every
// tool call and rule sets are small + stable.
var globCache sync.Map // pattern string → *regexp.Regexp (nil = invalid)

// matchGlob implements gitignore-style globs as a full-string match.
func matchGlob(pattern, input string) bool {
	var re *regexp.Regexp
	if v, ok := globCache.Load(pattern); ok {
		re, _ = v.(*regexp.Regexp)
	} else {
		re = compileGlob(pattern)
		globCache.Store(pattern, re)
	}
	if re == nil {
		return false // invalid pattern never matches (fail safe → ASK)
	}
	return re.MatchString(input)
}

// compileGlob translates a glob into an anchored regexp. Returns nil
// when the pattern can't compile.
func compileGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`\A`)
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(`.*`) // ** crosses path separators
				i++
				// swallow one following slash so "/a/**/b" matches "/a/b"
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					b.WriteString(`/?`)
					i++
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			// Pass char classes through verbatim up to the closing ].
			j := strings.IndexByte(pattern[i:], ']')
			if j < 0 {
				b.WriteString(regexp.QuoteMeta(string(c)))
				continue
			}
			b.WriteString(pattern[i : i+j+1])
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
