package security

import (
	"strings"
	"testing"
)

// ─── Layer 1: SENSITIVE_TOKEN_RE (JSON token-key matcher) ────────

func TestRedact_AccessTokenJSON(t *testing.T) {
	in := `{"access_token":"sk-real-secret-12345","other":"keep"}`
	out := Redact(in)
	if strings.Contains(out, "sk-real-secret-12345") {
		t.Errorf("access_token value leaked: %s", out)
	}
	if !strings.Contains(out, `"access_token":"[REDACTED]"`) {
		t.Errorf("expected access_token redacted, got: %s", out)
	}
	if !strings.Contains(out, `"other":"keep"`) {
		t.Errorf("non-sensitive keys must survive, got: %s", out)
	}
}

func TestRedact_RefreshIdSubjectAssertion(t *testing.T) {
	cases := []string{"refresh_token", "id_token", "subject_token", "assertion", "client_secret", "client_assertion"}
	for _, key := range cases {
		in := `{"` + key + `":"DEADBEEF12345"}`
		out := Redact(in)
		if strings.Contains(out, "DEADBEEF12345") {
			t.Errorf("%s value leaked: %s", key, out)
		}
		if !strings.Contains(out, `"`+key+`":"[REDACTED]"`) {
			t.Errorf("%s should be redacted: got %s", key, out)
		}
	}
}

func TestRedact_NestedJSON(t *testing.T) {
	// Nested envelope from an OAuth error response.
	in := `{"error":"invalid_grant","details":{"subject_token":"shouldNotLeak"}}`
	out := Redact(in)
	if strings.Contains(out, "shouldNotLeak") {
		t.Errorf("nested subject_token leaked: %s", out)
	}
}

func TestRedact_WhitespaceAroundColon(t *testing.T) {
	// Pretty-printed JSON has spaces / newlines around `:`.
	in := "{\n  \"access_token\" :   \"secret-with-spaces\"\n}"
	out := Redact(in)
	if strings.Contains(out, "secret-with-spaces") {
		t.Errorf("pretty-printed token leaked: %s", out)
	}
}

// ─── Layer 2: gitleaks bare-prefix patterns ──────────────────────

func TestRedact_GitHubPAT(t *testing.T) {
	pat := "ghp_" + strings.Repeat("a", 36)
	in := "Authorization: token " + pat + "\n"
	out := Redact(in)
	if strings.Contains(out, pat) {
		t.Errorf("GitHub PAT leaked: %s", out)
	}
}

func TestRedact_GitHubFineGrained(t *testing.T) {
	pat := "github_pat_" + strings.Repeat("X", 82)
	in := "secret = " + pat + " end"
	out := Redact(in)
	if strings.Contains(out, pat) {
		t.Errorf("github fine-grained PAT leaked: %s", out)
	}
}

func TestRedact_OpenAILegacyKey(t *testing.T) {
	key := "sk-" + strings.Repeat("a", 20) + "T3BlbkFJ" + strings.Repeat("b", 20)
	in := "OPENAI_API_KEY=" + key
	out := Redact(in)
	if strings.Contains(out, key) {
		t.Errorf("OpenAI legacy key leaked: %s", out)
	}
}

func TestRedact_AnthropicAPIKey(t *testing.T) {
	// 93 chars then AA suffix per real shape
	key := "sk-" + "ant-api03-" + strings.Repeat("X", 93) + "AA"
	in := "key: " + key + "\n"
	out := Redact(in)
	if strings.Contains(out, key) {
		t.Errorf("Anthropic key leaked: %s", out)
	}
}

func TestRedact_AWSAccessKey(t *testing.T) {
	key := "AKIA" + strings.Repeat("A", 16) // matches AKIA + [A-Z2-7]{16}
	in := "AWS_ACCESS_KEY=" + key
	out := Redact(in)
	if strings.Contains(out, key) {
		t.Errorf("AWS key leaked: %s", out)
	}
}

func TestRedact_GCPAPIKey(t *testing.T) {
	key := "AIza" + strings.Repeat("a", 35)
	in := "key=" + key
	out := Redact(in)
	if strings.Contains(out, key) {
		t.Errorf("GCP API key leaked: %s", out)
	}
}

func TestRedact_BearerToken(t *testing.T) {
	tok := strings.Repeat("X", 40)
	in := "Authorization: Bearer " + tok + "\n"
	out := Redact(in)
	if strings.Contains(out, tok) {
		t.Errorf("bearer token leaked: %s", out)
	}
	if !strings.Contains(out, "Bearer [REDACTED]") {
		t.Errorf("bearer prefix should survive, got: %s", out)
	}
}

func TestRedact_PrivateKeyPEM(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIEpAIBAAKCAQEA", 8) + "\n" +
		"-----END RSA PRIVATE KEY-----"
	in := "key:\n" + pem + "\nend"
	out := Redact(in)
	if strings.Contains(out, "MIIEpAIBAAKCAQEA") {
		t.Errorf("private key body leaked: %s", out)
	}
}

func TestRedact_StripeKey(t *testing.T) {
	for _, p := range []string{"sk_live_", "sk_test_", "rk_test_", "sk_prod_"} {
		key := p + strings.Repeat("a", 50)
		in := "stripe=" + key
		out := Redact(in)
		if strings.Contains(out, key) {
			t.Errorf("stripe %s key leaked: %s", p, out)
		}
	}
}

// ─── No-match preservation ───────────────────────────────────────

func TestRedact_PlainTextUnchanged(t *testing.T) {
	plain := "This is just some user message about implementing a feature, no secrets at all."
	out := Redact(plain)
	if out != plain {
		t.Errorf("plain text should be unchanged:\n  in:  %q\n  out: %q", plain, out)
	}
}

func TestRedact_EmptyInput(t *testing.T) {
	if out := Redact(""); out != "" {
		t.Errorf("empty in → empty out, got %q", out)
	}
}

// "ghp_" without 36-char tail should NOT match (the regex requires
// exactly 36 base62 chars).
func TestRedact_PrefixWithoutPayloadStays(t *testing.T) {
	in := "the prefix ghp_ alone is fine"
	out := Redact(in)
	if out != in {
		t.Errorf("prefix-only should not redact: %q → %q", in, out)
	}
}

// ─── Scan diagnostic ─────────────────────────────────────────────

func TestScan_DetectsAndLabels(t *testing.T) {
	in := "ghp_" + strings.Repeat("a", 36) + " and AIza" + strings.Repeat("b", 35)
	matches := Scan(in)
	if len(matches) < 2 {
		t.Fatalf("expected ≥ 2 matches, got %d: %+v", len(matches), matches)
	}
	gotIDs := map[string]bool{}
	gotLabels := map[string]bool{}
	for _, m := range matches {
		gotIDs[m.RuleID] = true
		gotLabels[m.Label] = true
	}
	if !gotIDs["github-pat"] {
		t.Error("missing github-pat in scan results")
	}
	if !gotIDs["gcp-api-key"] {
		t.Error("missing gcp-api-key in scan results")
	}
	// Labels should be human-readable.
	if !gotLabels["GitHub PAT"] {
		t.Error("missing GitHub PAT label")
	}
	if !gotLabels["GCP API Key"] {
		t.Errorf("missing GCP API Key label, got: %v", gotLabels)
	}
}

func TestScan_DedupesByRuleID(t *testing.T) {
	// Two GitHub PATs should still report exactly one match (we
	// dedupe per rule, not per occurrence — the goal is "are there
	// any of this kind", not "count them").
	pat1 := "ghp_" + strings.Repeat("a", 36)
	pat2 := "ghp_" + strings.Repeat("b", 36)
	matches := Scan(pat1 + " " + pat2)
	count := 0
	for _, m := range matches {
		if m.RuleID == "github-pat" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 dedupe'd github-pat match, got %d", count)
	}
}

// ─── Label conversion ────────────────────────────────────────────

func TestRuleIDLabel_SpecialCasings(t *testing.T) {
	cases := []struct{ id, want string }{
		{"github-pat", "GitHub PAT"},
		{"aws-access-token", "AWS Access Token"},
		{"gcp-api-key", "GCP API Key"},
		{"openai-api-key", "OpenAI API Key"},
		{"digitalocean-pat", "DigitalOcean PAT"},
		{"huggingface-access-token", "HuggingFace Access Token"},
		{"private-key-pem", "Private Key PEM"},
		{"sendgrid-api-token", "SendGrid API Token"},
	}
	for _, c := range cases {
		if got := ruleIDLabel(c.id); got != c.want {
			t.Errorf("%s → %q, want %q", c.id, got, c.want)
		}
	}
}
