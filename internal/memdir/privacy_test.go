package memdir

import (
	"strings"
	"testing"
)

// TestRedact_NoSecretsPassesThrough — the 99% case: a memo with no
// detectable secrets returns the input unchanged, no hits.
func TestRedact_NoSecretsPassesThrough(t *testing.T) {
	in := "User prefers TypeScript for new projects. Has 10 years Go background."
	res := Redact(in)
	if res.Reject {
		t.Errorf("benign text should not be rejected; got Reject=true")
	}
	if res.Redacted != in {
		t.Errorf("benign text mutated:\n in: %q\nout: %q", in, res.Redacted)
	}
	if len(res.Hits) != 0 {
		t.Errorf("benign text produced hits: %v", res.Hits)
	}
}

// TestRedact_TavilyKey — the Tavily key the user just pasted in our
// conversation is the exact shape we need to catch. Verify it
// matches and that the rest of the surrounding text survives.
func TestRedact_TavilyKey(t *testing.T) {
	in := "Set TAVILY_API_KEY to tvly-dev-FAKE-TEST-FIXTURE-NOT-A-REAL-KEY-Aa1Bb2Cc3Dd4Ee5 for the search backend."
	res := Redact(in)
	if res.Reject {
		t.Fatalf("single-key memo should redact, not reject; got Reject=true")
	}
	if strings.Contains(res.Redacted, "tvly-dev-FAKE") {
		t.Errorf("Tavily key leaked through:\n%s", res.Redacted)
	}
	// env-assign + tavily patterns could both match the same token.
	// At minimum one of them must have fired.
	total := 0
	for _, n := range res.Hits {
		total += n
	}
	if total == 0 {
		t.Errorf("expected at least 1 hit; got none. Output: %q", res.Redacted)
	}
}

// TestRedact_OpenAIKey — sk-... shape, the canonical secret string.
func TestRedact_OpenAIKey(t *testing.T) {
	in := "Run with OPENAI_API_KEY=sk-proj-AbCdEf123456789ABCDEFGHijklmno to enable GPT-5."
	res := Redact(in)
	if strings.Contains(res.Redacted, "sk-proj-AbC") {
		t.Errorf("OpenAI key leaked: %s", res.Redacted)
	}
}

// TestRedact_JWT — three-segment base64url with eyJ prefix.
func TestRedact_JWT(t *testing.T) {
	in := "Header: Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	res := Redact(in)
	if strings.Contains(res.Redacted, "eyJhbGciOi") {
		t.Errorf("JWT leaked: %s", res.Redacted)
	}
	if res.Hits["jwt"] == 0 {
		t.Errorf("expected jwt hit; got %v", res.Hits)
	}
}

// TestRedact_GitHubPAT — gh[pousr]_ + 36 alnum.
func TestRedact_GitHubPAT(t *testing.T) {
	in := "Token: ghp_a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8 grants repo access."
	res := Redact(in)
	if strings.Contains(res.Redacted, "ghp_a1B2") {
		t.Errorf("GH PAT leaked: %s", res.Redacted)
	}
}

// TestRedact_EnvAssignPreservesName — `<NAME>_API_KEY=<value>`
// pattern: NAME stays, VALUE redacted. Preserves the semantic that
// "user mentioned config for X" without leaking material.
func TestRedact_EnvAssignPreservesName(t *testing.T) {
	in := "Set CUSTOM_API_KEY=zXcVbNmAsDfGhJkL1234567890abcdef in your shell rc."
	res := Redact(in)
	if !strings.Contains(res.Redacted, "CUSTOM_API_KEY=") {
		t.Errorf("env-assign should preserve NAME prefix; got: %s", res.Redacted)
	}
	if strings.Contains(res.Redacted, "zXcVbNm") {
		t.Errorf("env-assign should redact VALUE; got: %s", res.Redacted)
	}
}

// TestRedact_TooManySecretsRejects — paste of an env file with 6+
// keys must hit the soft cap and produce Reject=true, so the memo
// isn't persisted at all.
func TestRedact_TooManySecretsRejects(t *testing.T) {
	in := strings.Join([]string{
		"OPENAI_API_KEY=sk-1234567890abcdefghijklmnop",
		"ANTHROPIC_API_KEY=sk-ant-1234567890abcdefghijklmnop",
		"GITHUB_TOKEN=ghp_a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8",
		"AWS_SECRET_ACCESS_KEY=zXcVbNmAsDfGhJkLqWeRtYuI",
		"TAVILY_API_KEY=tvly-dev-FAKE-TEST-FIXTURE-XX1abcdefghij",
		"BRAVE_SEARCH_API_KEY=BSAabcdefghijklmnopqrstuv",
		"SLACK_TOKEN=xoxb-1234-5678-aaaaaaaaaaaaaaaa",
	}, "\n")
	res := Redact(in)
	if !res.Reject {
		t.Errorf("env dump with 7+ keys should Reject; got Redacted=%q", res.Redacted)
	}
	total := 0
	for _, n := range res.Hits {
		total += n
	}
	if total <= MaxSecretsBeforeReject {
		t.Errorf("total hits = %d, want > %d", total, MaxSecretsBeforeReject)
	}
}

// TestRedact_PEMBlockMarker — PEM begin marker alone is enough to
// trigger the kind (we don't want to be in the business of finding
// the end of the key material in raw text; reject the whole memo).
func TestRedact_PEMBlockMarker(t *testing.T) {
	in := "User pasted: -----BEGIN RSA PRIVATE KEY----- followed by a long base64 blob."
	res := Redact(in)
	if res.Hits["pem"] == 0 {
		t.Errorf("PEM marker not caught; hits=%v", res.Hits)
	}
	if strings.Contains(res.Redacted, "-----BEGIN RSA") {
		t.Errorf("PEM marker leaked: %s", res.Redacted)
	}
}

// TestRedact_NoFalsePositiveOnGitSHA — a 40-char hex git SHA must
// NOT be flagged (would trigger on every CHANGELOG memo otherwise).
// The patterns require structural prefixes (sk-, gh*_, AKIA…) so
// bare hex passes through.
func TestRedact_NoFalsePositiveOnGitSHA(t *testing.T) {
	in := "Fixed in commit a1b2c3d4e5f67890abcdef1234567890abcdef12 — see release-0520."
	res := Redact(in)
	if res.Reject {
		t.Errorf("git SHA should not reject memo")
	}
	if strings.Contains(res.Redacted, "[REDACTED") {
		t.Errorf("git SHA falsely redacted: %s", res.Redacted)
	}
}

// TestRedact_NoFalsePositiveOnPathOrUUID — common dev strings that
// look secret-ish but aren't: file paths, UUIDs in URLs, base64-ish
// hash digests in technical docs.
func TestRedact_NoFalsePositiveOnPathOrUUID(t *testing.T) {
	cases := []string{
		"Cache file at /var/cache/myapp/9f1c5dc6-9234-4f12-aaaa-bcdef0123456.bin",
		"npm SHA: sha256-aBcDeFgHiJkLmNoPqRsTuVwXyZ1234567890+/=",
		"Tracking ID anonymous-user-87b2c5a4f6e8d9c0b1a2 in logs.",
	}
	for _, in := range cases {
		res := Redact(in)
		if res.Reject {
			t.Errorf("input falsely rejected: %q", in)
		}
		if strings.Contains(res.Redacted, "[REDACTED") {
			t.Errorf("input falsely redacted:\n  in:  %q\n  out: %q", in, res.Redacted)
		}
	}
}

// TestHitsSummary_Format — stable single-line format for debug log.
func TestHitsSummary_Format(t *testing.T) {
	got := HitsSummary(map[string]int{"openai": 2, "jwt": 1})
	// Order follows secretPatterns declaration order (jwt before
	// openai), so jwt=1 openai=2.
	want := "jwt=1 openai=2"
	if got != want {
		t.Errorf("HitsSummary = %q, want %q", got, want)
	}
	if HitsSummary(nil) != "(none)" {
		t.Errorf("nil hits should format as (none); got %q", HitsSummary(nil))
	}
}
