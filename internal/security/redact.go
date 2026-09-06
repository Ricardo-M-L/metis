package security

// redact.go — secret-pattern redaction for any byte stream metis is
// about to write to disk (HTTP request/response dumps in transport/
// dump.go, session JSONL in session/session.go, debug logs).
//
// Two layers, mirroring claude-code-sourcemap:
//
//  1. SENSITIVE_TOKEN_RE (xaa.ts) — quoted-JSON-key matcher for OAuth
//     token-bearing fields. Catches `"access_token":"..."` no matter
//     how deeply nested. The pattern matches both stringified bodies
//     AND raw text() error responses, so a misbehaving OAuth server
//     that echoes the request's `subject_token` in a 4xx body doesn't
//     leak it into our debug log.
//
//  2. Gitleaks-derived high-confidence rules (secretScanner.ts) — known
//     credential prefixes with near-zero false-positive rates. Keeps
//     only rules where the pattern is so distinctive that random user
//     content can't accidentally fire (e.g. `ghp_<36 base62>`,
//     `sk-ant-api03-…AA`, `dapi<32hex>`, `AIza…`). Skips generic
//     keyword-context rules ("password = ..." style) which produce
//     too many false hits.
//
// Both layers replace the matched secret span with `[REDACTED]` while
// preserving surrounding bytes. Order matters: layer 1 runs first
// (most aggressive on JSON), then layer 2 (catches bare tokens in
// non-JSON bodies, response headers leaked into bodies, etc.).
//
// Usage:
//   redacted := security.Redact(string(rawBody))
//   _ = os.WriteFile(path, []byte(redacted), 0o600)
//
// Source rule IDs and regex source: gitleaks (MIT license)
// https://github.com/gitleaks/gitleaks/blob/master/config/gitleaks.toml

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// sensitiveTokenRE matches `"<token-key>": "<value>"` where <token-key>
// is one of the OAuth flow's secret-bearing fields. The replace below
// keeps the key intact and clobbers only the value.
//
// (?s) lets `.` cross newlines — some servers pretty-print bodies with
// `\n` between key and value. \s* matches the optional whitespace JSON
// allows around `:`.
var sensitiveTokenRE = regexp.MustCompile(`"(access_token|refresh_token|id_token|assertion|subject_token|client_secret|client_assertion|chatgpt_account_id)"\s*:\s*"(?:\\.|[^"\\\r\n])*"`)

// secretRule is one gitleaks-derived pattern. capturing=true means the
// pattern's group 1 is the secret span (and surrounding boundary
// chars must be preserved); capturing=false means the entire match is
// the secret.
type secretRule struct {
	id        string
	source    string
	capturing bool
	// captureGroup defaults to 1 for capturing rules. It is configurable for
	// patterns that must consume a leading boundary because Go's regexp engine
	// intentionally does not implement look-behind.
	captureGroup      int
	base64URLBoundary bool
}

// secretRules — curated high-confidence subset of gitleaks rules.
// Each pattern was selected because:
//
//   - It has a distinctive prefix (sk-/ghp_/dapi/AIza/...)
//   - Length is fixed or has a tight upper bound
//   - The rule catches the canonical real-world token shape, not a
//     keyword-context heuristic
//
// These together cover ~90% of "secret accidentally pasted into prompt
// or echoed in response" scenarios without the false-positive noise
// of gitleaks's full ruleset.
var secretRules = []secretRule{
	// — Cloud providers —
	{id: "aws-access-token", source: `\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16})\b`, capturing: true},
	{id: "gcp-api-key", source: `\b(AIza[\w-]{35})\b`, capturing: true},
	{id: "digitalocean-pat", source: `\b(dop_v1_[a-f0-9]{64})\b`, capturing: true},
	{id: "digitalocean-access-token", source: `\b(doo_v1_[a-f0-9]{64})\b`, capturing: true},

	// — AI APIs (the metis-most-relevant ones) —
	// Anthropic key prefix split via concat to avoid the literal byte
	// sequence appearing in this binary's strings dump (cosmetic).
	{id: "anthropic-api-key", source: `\b(sk-` + `ant-api03-[a-zA-Z0-9_\-]{93}AA)\b`, capturing: true},
	{id: "anthropic-admin-api-key", source: `\b(sk-` + `ant-admin01-[a-zA-Z0-9_\-]{93}AA)\b`, capturing: true},
	{id: "openai-api-key", source: `\b(sk-(?:proj|svcacct|admin)-(?:[A-Za-z0-9_-]{74}|[A-Za-z0-9_-]{58})T3BlbkFJ(?:[A-Za-z0-9_-]{74}|[A-Za-z0-9_-]{58}))\b`, capturing: true},
	{id: "openai-legacy-api-key", source: `\b(sk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20})\b`, capturing: true},
	{id: "huggingface-access-token", source: `\b(hf_[a-zA-Z]{34})\b`, capturing: true},

	// — Version control —
	{id: "github-pat", source: `\b(ghp_[0-9a-zA-Z]{36})\b`, capturing: true},
	{id: "github-fine-grained-pat", source: `\b(github_pat_\w{82})\b`, capturing: true},
	{id: "github-app-token", source: `\b((?:ghu|ghs)_[0-9a-zA-Z]{36})\b`, capturing: true},
	{id: "github-oauth", source: `\b(gho_[0-9a-zA-Z]{36})\b`, capturing: true},
	{id: "github-refresh-token", source: `\b(ghr_[0-9a-zA-Z]{36})\b`, capturing: true},
	{id: "gitlab-pat", source: `\b(glpat-[\w-]{20})\b`, capturing: true},
	{id: "gitlab-deploy-token", source: `\b(gldt-[0-9a-zA-Z_\-]{20})\b`, capturing: true},

	// — Communication —
	{id: "slack-bot-token", source: `xoxb-[0-9]{10,13}-[0-9]{10,13}[a-zA-Z0-9-]*`, capturing: false},
	{id: "slack-user-token", source: `xox[pe](?:-[0-9]{10,13}){3}-[a-zA-Z0-9-]{28,34}`, capturing: false},
	{id: "twilio-api-key", source: `SK[0-9a-fA-F]{32}`, capturing: false},
	{id: "sendgrid-api-token", source: `\b(SG\.[a-zA-Z0-9=_\-.]{66})\b`, capturing: true},

	// — Dev tooling —
	{id: "npm-access-token", source: `\b(npm_[a-zA-Z0-9]{36})\b`, capturing: true},
	{id: "pypi-upload-token", source: `pypi-AgEIcHlwaS5vcmc[\w-]{50,1000}`, capturing: false},
	{id: "databricks-api-token", source: `\b(dapi[a-f0-9]{32}(?:-\d)?)\b`, capturing: true},
	{id: "pulumi-api-token", source: `\b(pul-[a-f0-9]{40})\b`, capturing: true},

	// — Observability —
	{id: "grafana-cloud-api-token", source: `\b(glc_[A-Za-z0-9+/]{32,400}={0,3})`, capturing: true},
	{id: "grafana-service-account-token", source: `\b(glsa_[A-Za-z0-9]{32}_[A-Fa-f0-9]{8})\b`, capturing: true},
	{id: "sentry-user-token", source: `\b(sntryu_[a-f0-9]{64})\b`, capturing: true},

	// — Payment / commerce —
	{id: "stripe-access-token", source: `\b((?:sk|rk)_(?:test|live|prod)_[a-zA-Z0-9]{10,99})\b`, capturing: true},
	{id: "shopify-access-token", source: `shpat_[a-fA-F0-9]{32}`, capturing: false},
	{id: "shopify-shared-secret", source: `shpss_[a-fA-F0-9]{32}`, capturing: false},

	// — Generic Authorization headers (catch-all for echoed bearer
	// tokens in 4xx error bodies). Bearer is a non-capturing literal
	// so group 1 = the secret only; ReplaceAll keeps "Bearer " in the
	// surrounding match while clobbering the captured token. 20-char
	// minimum tail rejects "Bearer X" prose mentions.
	{id: "bearer-token", source: `(?i)(?:Bearer\s+)([A-Za-z0-9._\-=]{20,})`, capturing: true},
	// Compact JWTs are frequently logged without an Authorization header. The
	// encoded JSON header prefix plus three bounded base64url segments keeps
	// this rule precise enough to avoid matching ordinary dotted identifiers.
	{id: "jwt", source: `(eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,})`, capturing: true, base64URLBoundary: true},

	// — Crypto private keys —
	{id: "private-key-pem", source: `-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----[\s\S-]{64,}?-----END[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----`, capturing: false},
}

// compiledRules holds the lazy-compiled regex set. Compilation is
// deferred to first Redact/Scan call so package init stays cheap.
var (
	compileOnce  sync.Once
	compiledList []compiledRule
)

type compiledRule struct {
	id                string
	re                *regexp.Regexp
	capturing         bool
	captureGroup      int
	base64URLBoundary bool
}

func compileRules() {
	compiledList = make([]compiledRule, 0, len(secretRules))
	for _, r := range secretRules {
		captureGroup := r.captureGroup
		if r.capturing && captureGroup == 0 {
			captureGroup = 1
		}
		compiledList = append(compiledList, compiledRule{
			id:                r.id,
			re:                regexp.MustCompile(r.source),
			capturing:         r.capturing,
			captureGroup:      captureGroup,
			base64URLBoundary: r.base64URLBoundary,
		})
	}
}

// Redact returns s with every detected secret span replaced by
// `[REDACTED]`. Layer 1 (sensitiveTokenRE) handles JSON token fields;
// layer 2 (secretRules) handles bare credential prefixes anywhere in
// the body.
//
// Pure function — safe to call on any string. Empty / no-match input
// returns the original unchanged (zero allocation in that path).
func Redact(s string) string {
	if s == "" {
		return s
	}
	hasSensitiveField := mayContainSensitiveTokenField(s)
	hasHighConfidenceShape := mayContainHighConfidenceSecret(s)
	if !hasSensitiveField && !hasHighConfidenceShape {
		return s
	}

	// Layer 1: JSON token-key matcher. Replace value-only, keep key.
	if hasSensitiveField {
		s = sensitiveTokenRE.ReplaceAllStringFunc(s, func(match string) string {
			// match looks like `"access_token":"abc"` — find the key by
			// re-matching to extract group 1. ReplaceAllString with
			// `"$1":"[REDACTED]"` is shorter but ReplaceAllStringFunc
			// makes the boundary handling explicit.
			sub := sensitiveTokenRE.FindStringSubmatch(match)
			if len(sub) < 2 {
				return `"[REDACTED]":"[REDACTED]"` // defensive
			}
			return `"` + sub[1] + `":"[REDACTED]"`
		})
	}

	// Layer 2: gitleaks-derived bare credential patterns. For
	// capturing rules, replace only group 1 so word-boundary chars
	// (spaces, quotes) outside the group survive.
	if !hasHighConfidenceShape {
		return s
	}
	compileOnce.Do(compileRules)
	for _, r := range compiledList {
		if !r.re.MatchString(s) {
			continue
		}
		if r.capturing {
			s = redactCapturedMatches(s, r)
		} else {
			s = r.re.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}

// RedactValues applies the high-confidence generic rules from Redact and then
// removes the exact opaque values supplied by the caller. The second layer is
// for credentials known only at request time (for example a vendor-specific
// API key, an OAuth access token, or an account id) whose shape cannot be
// recognized safely by a global regular expression.
//
// In plain text, exact values are first swapped for collision-free placeholders,
// then Redact runs, and finally the placeholders become [REDACTED]. This keeps
// the public ordering contract (generic rules, then exact-value markers) while
// preventing a generic rule from partially consuming an exact value before it
// can be removed. Longest-first matching also handles overlapping values.
// Valid JSON is matched inside decoded string literals so alternate JSON
// escapes cannot hide a credential or be mistaken for one. Unchanged literals
// and all non-string JSON bytes are preserved.
func RedactValues(s string, values ...string) string {
	if s == "" || len(values) == 0 {
		return Redact(s)
	}

	trimmed := strings.TrimSpace(s)
	jsonInput := len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') && json.Valid([]byte(s))
	escapedJSON := jsonInput && strings.Contains(s, `\`)
	unique := make(map[string]struct{}, len(values))
	exact := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value == "[REDACTED]" || (!escapedJSON && !strings.Contains(s, value)) {
			continue
		}
		if _, ok := unique[value]; ok {
			continue
		}
		unique[value] = struct{}{}
		exact = append(exact, value)
	}
	if len(exact) == 0 {
		return Redact(s)
	}
	sort.SliceStable(exact, func(i, j int) bool { return len(exact[i]) > len(exact[j]) })
	if jsonInput {
		return Redact(redactJSONExactValues(s, exact))
	}

	prefix := "\x00metis-exact-secret:"
	for strings.Contains(s, prefix) {
		prefix += "_"
	}
	maskPairs := make([]string, 0, len(exact)*2)
	restorePairs := make([]string, 0, len(exact)*2)
	for i, value := range exact {
		placeholder := prefix + strconv.Itoa(i) + "\x00"
		maskPairs = append(maskPairs, value, placeholder)
		restorePairs = append(restorePairs, placeholder, "[REDACTED]")
	}
	masked := strings.NewReplacer(maskPairs...).Replace(s)
	masked = Redact(masked)
	return strings.NewReplacer(restorePairs...).Replace(masked)
}

// redactJSONExactValues receives valid JSON and rewrites only string literals
// containing an exact value. It avoids decoding the document into maps, which
// would reorder keys, collapse duplicates, and change number representations.
func redactJSONExactValues(s string, exact []string) string {
	pairs := make([]string, 0, 2*len(exact))
	for _, value := range exact {
		pairs = append(pairs, value, "[REDACTED]")
	}
	replacer := strings.NewReplacer(pairs...)
	var out strings.Builder
	last := 0
	for pos := 0; pos < len(s); pos++ {
		if s[pos] != '"' {
			continue
		}
		start := pos
		for pos++; pos < len(s); pos++ {
			if s[pos] == '\\' {
				pos++
			} else if s[pos] == '"' {
				break
			}
		}
		literal := s[start : pos+1]
		decoded := literal[1 : len(literal)-1]
		if strings.Contains(decoded, `\`) {
			if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
				continue // Defensive: the caller has already validated the JSON.
			}
		}
		redacted := replacer.Replace(decoded)
		if redacted == decoded {
			continue
		}
		encoded, _ := json.Marshal(redacted)
		out.WriteString(s[last:start])
		out.Write(encoded)
		last = pos + 1
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

func redactCapturedMatches(s string, rule compiledRule) string {
	matches := rule.re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	groupOffset := rule.captureGroup * 2
	out := make([]byte, 0, len(s))
	last := 0
	redacted := false
	for _, match := range matches {
		if groupOffset <= 1 || groupOffset+1 >= len(match) {
			continue
		}
		start, end := match[groupOffset], match[groupOffset+1]
		if start < last || start < 0 || end < start {
			continue
		}
		if rule.base64URLBoundary {
			if start > 0 && isBase64URLByte(s[start-1]) {
				continue
			}
			if end < len(s) && isBase64URLByte(s[end]) {
				continue
			}
		}
		out = append(out, s[last:start]...)
		out = append(out, "[REDACTED]"...)
		last = end
		redacted = true
	}
	if !redacted {
		return s
	}
	out = append(out, s[last:]...)
	return string(out)
}

func isBase64URLByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '-' || b == '_'
}

func compiledRuleMatches(s string, rule compiledRule) bool {
	if !rule.base64URLBoundary {
		return rule.re.MatchString(s)
	}
	groupOffset := rule.captureGroup * 2
	for _, match := range rule.re.FindAllStringSubmatchIndex(s, -1) {
		if groupOffset <= 1 || groupOffset+1 >= len(match) {
			continue
		}
		start, end := match[groupOffset], match[groupOffset+1]
		if start < 0 || end < start {
			continue
		}
		if start > 0 && isBase64URLByte(s[start-1]) {
			continue
		}
		if end < len(s) && isBase64URLByte(s[end]) {
			continue
		}
		return true
	}
	return false
}

func mayContainSensitiveTokenField(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		for _, name := range []string{
			"access_token", "refresh_token", "id_token", "assertion",
			"subject_token", "client_secret", "client_assertion",
			"chatgpt_account_id",
		} {
			if hasStringPrefixAt(s, i+1, name) {
				return true
			}
		}
	}
	return false
}

func mayContainHighConfidenceSecret(s string) bool {
	for i := 0; i < len(s); i++ {
		switch lowerASCIIByte(s[i]) {
		case 'a':
			for _, prefix := range []string{"a3t", "akia", "asia", "abia", "acca", "aiza"} {
				if hasASCIIFoldPrefixAt(s, i, prefix) {
					return true
				}
			}
		case 'b':
			if hasASCIIFoldPrefixAt(s, i, "bearer") {
				return true
			}
		case 'd':
			for _, prefix := range []string{"dop_v1_", "doo_v1_", "dapi"} {
				if hasStringPrefixAt(s, i, prefix) {
					return true
				}
			}
		case 'e':
			if hasASCIIFoldPrefixAt(s, i, "eyj") {
				return true
			}
		case 'g':
			for _, prefix := range []string{
				"ghp_", "github_pat_", "ghu_", "ghs_", "gho_", "ghr_",
				"glpat-", "gldt-", "glc_", "glsa_",
			} {
				if hasStringPrefixAt(s, i, prefix) {
					return true
				}
			}
		case 'h':
			if hasStringPrefixAt(s, i, "hf_") {
				return true
			}
		case 'n':
			if hasStringPrefixAt(s, i, "npm_") {
				return true
			}
		case 'p':
			if hasStringPrefixAt(s, i, "pypi-") || hasStringPrefixAt(s, i, "pul-") {
				return true
			}
		case 'r':
			if hasStringPrefixAt(s, i, "rk_") {
				return true
			}
		case 's':
			for _, prefix := range []string{"sk-", "sk_", "sg.", "shpat_", "shpss_", "sntryu_"} {
				if hasASCIIFoldPrefixAt(s, i, prefix) {
					return true
				}
			}
			// Twilio keys use uppercase SK followed by 32 hex digits.
			if s[i] == 'S' && hasStringPrefixAt(s, i, "SK") {
				return true
			}
		case 'x':
			if hasStringPrefixAt(s, i, "xox") {
				return true
			}
		case '-':
			if hasStringPrefixAt(s, i, "-----BEGIN") {
				return true
			}
		}
	}
	return false
}

func hasStringPrefixAt(s string, start int, prefix string) bool {
	return start >= 0 && start+len(prefix) <= len(s) && s[start:start+len(prefix)] == prefix
}

func hasASCIIFoldPrefixAt(s string, start int, prefix string) bool {
	if start < 0 || start+len(prefix) > len(s) {
		return false
	}
	for i := range prefix {
		if lowerASCIIByte(s[start+i]) != lowerASCIIByte(prefix[i]) {
			return false
		}
	}
	return true
}

func lowerASCIIByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// SecretMatch — diagnostic shape returned by Scan. Useful in CLI
// commands that warn about secrets without redacting them
// (e.g. "your config contains 2 secret(s): GitHub PAT, OpenAI API
// Key — review before sharing"). The actual matched text is NOT
// returned — we never log secret values.
type SecretMatch struct {
	RuleID string
	Label  string
}

// Scan returns one SecretMatch per rule that fires (deduplicated by
// rule id). Use for diagnostics; use Redact to actually clean a body.
func Scan(s string) []SecretMatch {
	if s == "" {
		return nil
	}
	compileOnce.Do(compileRules)
	var out []SecretMatch
	seen := map[string]struct{}{}
	for _, r := range compiledList {
		if _, ok := seen[r.id]; ok {
			continue
		}
		if compiledRuleMatches(s, r) {
			seen[r.id] = struct{}{}
			out = append(out, SecretMatch{
				RuleID: r.id,
				Label:  ruleIDLabel(r.id),
			})
		}
	}
	return out
}

// ruleIDLabel turns "github-pat" into "GitHub PAT" etc. Matches
// claude-code's ruleIdToLabel — same special-case map.
func ruleIDLabel(id string) string {
	specials := map[string]string{
		"aws":          "AWS",
		"gcp":          "GCP",
		"api":          "API",
		"pat":          "PAT",
		"oauth":        "OAuth",
		"npm":          "NPM",
		"pypi":         "PyPI",
		"github":       "GitHub",
		"gitlab":       "GitLab",
		"openai":       "OpenAI",
		"digitalocean": "DigitalOcean",
		"huggingface":  "HuggingFace",
		"sendgrid":     "SendGrid",
		"pem":          "PEM",
	}
	parts := splitOn(id, '-')
	for i, p := range parts {
		if v, ok := specials[p]; ok {
			parts[i] = v
		} else if len(p) > 0 {
			parts[i] = capitalizeASCII(p)
		}
	}
	return joinOn(parts, ' ')
}

func splitOn(s string, sep byte) []string {
	var out []string
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[last:i])
			last = i + 1
		}
	}
	return append(out, s[last:])
}

func joinOn(parts []string, sep byte) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(parts) - 1
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			b = append(b, sep)
		}
		b = append(b, p...)
	}
	return string(b)
}

func capitalizeASCII(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}
