package memdir

// privacy.go — pre-write secret redactor for auto-memory.
//
// The threat: the auto-memory extractor reads the same conversation
// the model just saw. If the user pasted an API key (env-var dump,
// `curl -H "Authorization: ..."`, .env example) the extractor will
// happily fact-distill it into a memo and persist it 0o644 on disk
// under ~/.metis/memory/. From there it leaks to:
//   - cloud backups / dropbox / icloud sync the user forgot about
//   - the next session's manifest, sent to whatever model is loaded
//   - screen-share / pair-programming where the user opens the file
//
// Mitigation here is **regex-based pre-write filter**: scan the
// outgoing memo body against a closed list of secret shapes; on a
// match either redact the matched span (replacing it with a
// `[REDACTED:<kind>]` marker) or — if too many matches — reject the
// whole memo so we don't end up with a memo whose body is 80%
// `[REDACTED]`.
//
// This is belt-and-suspenders, not a security boundary. A motivated
// secret that doesn't match the patterns slips through; a future
// pattern update catches it. The cost is essentially zero (regex
// on ~1 KiB of text per write) and the failure mode is recoverable
// (worst case a real-looking string gets falsely redacted; the user
// re-runs without the trigger phrase).
//
// Patterns adapted from agentmemory's privacy filter
// (/Users/ricardo/Documents/公司学习文件/opensource-contributions/
// agentmemory/src/functions/privacy.ts) plus the metis-specific
// shapes for our own ecosystem (tavily, brave, etc.).

import (
	"regexp"
	"strings"
)

// secretPattern is one named regex in the redactor's pipeline.
// Order matters: longer/more-specific patterns first so a generic
// catch-all doesn't steal a match a specific provider could have
// labelled (e.g. an OpenAI sk-... shouldn't be labelled generic).
type secretPattern struct {
	kind string         // short label that lands in the [REDACTED:<kind>] marker
	re   *regexp.Regexp // compiled at init; must use raw string for clarity
}

// Each pattern intentionally requires enough structure (prefixes,
// segment count, length range) that random English text doesn't
// collide. We anchor with \b word boundaries where helpful; raw
// length-only heuristics (e.g. "any 40-char [a-z0-9] run") are
// avoided because they false-positive on git SHAs, commit ids,
// /proc paths, etc.
var secretPatterns = []secretPattern{
	// JWT — three base64url segments separated by dots. eyJ prefix is
	// the base64-encoded `{"` header opener; near-universal.
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},

	// OpenAI keys: sk-... and sk-proj-... (project keys, newer).
	{kind: "openai", re: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`)},

	// Anthropic: sk-ant-... including api key v0+v1 shapes.
	{kind: "anthropic", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},

	// GitHub PAT family: ghp_, gho_, ghu_, ghs_, ghr_ + 36 alnum.
	{kind: "github", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36}\b`)},

	// AWS access key. Fixed 20-char shape, AKIA / ASIA prefixes.
	{kind: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},

	// Google API key: AIza + 35 alnum/_/-.
	{kind: "google", re: regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}\b`)},

	// Tavily — tvly-... (covers tvly-dev-... too).
	{kind: "tavily", re: regexp.MustCompile(`\btvly-(?:dev-)?[A-Za-z0-9_-]{20,}\b`)},

	// Brave Search — BSA + 20+ alnum.
	{kind: "brave", re: regexp.MustCompile(`\bBSA[A-Za-z0-9_-]{20,}\b`)},

	// Serper — recognisable enough by the literal prefix.
	{kind: "serper", re: regexp.MustCompile(`\bserper[_-]?[Aa]pi[_-]?[Kk]ey[\s:=]+[A-Za-z0-9]{20,}\b`)},

	// Slack tokens — xox[bopa]-<numeric segments>-<alnum tail>.
	{kind: "slack", re: regexp.MustCompile(`\bxox[abopr]-[A-Za-z0-9-]{10,}\b`)},

	// .env-style assignments. `<NAME>_API_KEY=<value>` /
	// `<NAME>_TOKEN=<value>` / `<NAME>_SECRET=<value>`. Captures the
	// VALUE only — leaves the NAME visible so the memo still
	// communicates "user mentioned a key for X" without leaking X's
	// material. Requires the value to look secret-ish (>= 16 chars,
	// no whitespace) so a literal `FOO_API_KEY=description here`
	// doesn't fire.
	{kind: "env-assign", re: regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9_]*(?:API_KEY|TOKEN|SECRET|PASSWORD|PASSWD|PRIVATE_KEY)\s*[:=]\s*)([A-Za-z0-9_./+=-]{16,})`)},

	// PEM-style block markers — full body is too long for our
	// max-redact policy, just nuke the BEGIN/END envelope so the
	// reject-too-many counter trips for the whole memo.
	{kind: "pem", re: regexp.MustCompile(`-----BEGIN [A-Z ]+ KEY-----`)},
}

// MaxSecretsBeforeReject is the soft cap on per-memo redactions
// before we give up and reject the whole memo. Tuned so a memo
// that happens to mention 1-2 keys (with explanation around them)
// can still survive after redaction, but a paste-of-an-env-file
// (6+ hits) gets rejected wholesale because the result would be
// 80% [REDACTED:env-assign] markers anyway.
const MaxSecretsBeforeReject = 5

// RedactResult is what the caller gets back. Reject = "don't write
// this memo at all", Redacted = the sanitised text, Hits = list of
// (kind, count) so the dream agent can log "auto-memory dropped
// 3 secrets from feedback_X.md" without surfacing the values.
type RedactResult struct {
	Reject   bool
	Redacted string
	Hits     map[string]int
}

// Redact runs every pattern over `text`, replacing matches with
// `[REDACTED:<kind>]` markers and counting hits. When the cumulative
// hit count exceeds MaxSecretsBeforeReject, Reject is set to true
// and Redacted is left empty (the caller skips the write entirely
// rather than persist a memo that's mostly redaction noise).
//
// The env-assign pattern is special-cased: only the VALUE group
// gets replaced, the NAME prefix (`MY_API_KEY=`) is preserved so
// the memo retains the semantic "agent mentioned MY_API_KEY"
// without the actual material.
//
// Performance: regexes are compiled at init via MustCompile;
// FindAllString allocates the match slice but on the typical 1-2 KiB
// memo body the per-call cost is sub-millisecond. No caching needed.
func Redact(text string) RedactResult {
	hits := map[string]int{}
	out := text
	total := 0
	for _, p := range secretPatterns {
		matches := p.re.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		hits[p.kind] += len(matches)
		total += len(matches)
		if p.kind == "env-assign" {
			out = p.re.ReplaceAllString(out, "${1}[REDACTED:env-assign]")
		} else {
			out = p.re.ReplaceAllString(out, "[REDACTED:"+p.kind+"]")
		}
	}
	if total > MaxSecretsBeforeReject {
		return RedactResult{Reject: true, Hits: hits}
	}
	if total == 0 {
		return RedactResult{Redacted: text, Hits: hits}
	}
	return RedactResult{Redacted: out, Hits: hits}
}

// HitsSummary formats the hits map as a short single-line string,
// e.g. "openai=2 jwt=1". For logging. Deterministic key order so
// debug-log diffs don't churn.
func HitsSummary(hits map[string]int) string {
	if len(hits) == 0 {
		return "(none)"
	}
	// Sorted iteration via the existing secretPatterns order keeps
	// output stable without reaching for the sort package.
	var parts []string
	for _, p := range secretPatterns {
		if n := hits[p.kind]; n > 0 {
			parts = append(parts, p.kind+"="+itoa(n))
		}
	}
	return strings.Join(parts, " ")
}

// itoa is a 0-alloc small-int formatter for HitsSummary. Avoids
// pulling strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
