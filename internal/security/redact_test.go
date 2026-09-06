package security

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestRedactFastPathCoversEverySecretRule(t *testing.T) {
	samples := map[string]string{
		"aws-access-token":              "AKIA" + strings.Repeat("A", 16),
		"gcp-api-key":                   "AIza" + strings.Repeat("a", 35),
		"digitalocean-pat":              "dop_v1_" + strings.Repeat("a", 64),
		"digitalocean-access-token":     "doo_v1_" + strings.Repeat("b", 64),
		"anthropic-api-key":             "sk-ant-api03-" + strings.Repeat("A", 93) + "AA",
		"anthropic-admin-api-key":       "sk-ant-admin01-" + strings.Repeat("B", 93) + "AA",
		"openai-api-key":                "sk-proj-" + strings.Repeat("A", 74) + "T3BlbkFJ" + strings.Repeat("B", 74),
		"openai-legacy-api-key":         "sk-" + strings.Repeat("a", 20) + "T3BlbkFJ" + strings.Repeat("b", 20),
		"huggingface-access-token":      "hf_" + strings.Repeat("a", 34),
		"github-pat":                    "ghp_" + strings.Repeat("a", 36),
		"github-fine-grained-pat":       "github_pat_" + strings.Repeat("A", 82),
		"github-app-token":              "ghu_" + strings.Repeat("a", 36),
		"github-oauth":                  "gho_" + strings.Repeat("a", 36),
		"github-refresh-token":          "ghr_" + strings.Repeat("a", 36),
		"gitlab-pat":                    "glpat-" + strings.Repeat("a", 20),
		"gitlab-deploy-token":           "gldt-" + strings.Repeat("a", 20),
		"slack-bot-token":               "xoxb-1234567890-1234567890-abcdef",
		"slack-user-token":              "xoxp-1234567890-1234567890-1234567890-" + strings.Repeat("a", 28),
		"twilio-api-key":                "SK" + strings.Repeat("a", 32),
		"sendgrid-api-token":            "SG." + strings.Repeat("a", 66),
		"npm-access-token":              "npm_" + strings.Repeat("a", 36),
		"pypi-upload-token":             "pypi-AgEIcHlwaS5vcmc" + strings.Repeat("a", 50),
		"databricks-api-token":          "dapi" + strings.Repeat("a", 32),
		"pulumi-api-token":              "pul-" + strings.Repeat("a", 40),
		"grafana-cloud-api-token":       "glc_" + strings.Repeat("A", 32),
		"grafana-service-account-token": "glsa_" + strings.Repeat("A", 32) + "_" + strings.Repeat("a", 8),
		"sentry-user-token":             "sntryu_" + strings.Repeat("a", 64),
		"stripe-access-token":           "sk_test_" + strings.Repeat("a", 20),
		"shopify-access-token":          "shpat_" + strings.Repeat("a", 32),
		"shopify-shared-secret":         "shpss_" + strings.Repeat("a", 32),
		"bearer-token":                  "Bearer " + strings.Repeat("A", 20),
		"jwt":                           "eyJ" + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12),
		"private-key-pem":               "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", 64) + "\n-----END PRIVATE KEY-----",
	}

	for _, rule := range secretRules {
		sample, ok := samples[rule.id]
		if !ok {
			t.Fatalf("missing representative sample for rule %q", rule.id)
		}
		if !regexp.MustCompile(rule.source).MatchString(sample) {
			t.Fatalf("sample for %q does not match its rule", rule.id)
		}
		if got := Redact(sample); got == sample {
			t.Fatalf("Redact fast path skipped rule %q", rule.id)
		}
	}
}

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
	cases := []string{
		"refresh_token", "id_token", "subject_token", "assertion",
		"client_secret", "client_assertion", "chatgpt_account_id",
	}
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

func TestRedact_DoesNotTreatOrdinaryAccessOrRefreshMetadataAsSecrets(t *testing.T) {
	in := `{"access":"granted","refresh":"manual","refresh_interval":30}`
	if out := Redact(in); out != in {
		t.Fatalf("ordinary access/refresh metadata changed: got %q, want %q", out, in)
	}
}

func TestRedactPreservesBusinessAccountIDAndProtectsKnownOAuthAccount(t *testing.T) {
	in := `{"event":"account_created","account_id":"fixture-account-123"}`
	if got := Redact(in); got != in {
		t.Fatalf("business account changed: got %q, want %q", got, in)
	}
	known := `{"account_id":"oauth-account-456","business":{"account_id":"fixture-account-123"}}`
	want := `{"account_id":"[REDACTED]","business":{"account_id":"fixture-account-123"}}`
	if got := RedactValues(known, "oauth-account-456"); got != want {
		t.Fatalf("known account redaction = %q, want %q", got, want)
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

func TestRedact_RawJWT(t *testing.T) {
	for _, suffix := range []string{"c", "-", "_"} {
		token := "eyJ" + strings.Repeat("a", 16) + "." + strings.Repeat("b", 16) + "." + strings.Repeat("c", 15) + suffix
		in := "token response: " + token + "; end"
		out := Redact(in)
		if strings.Contains(out, token) || !strings.Contains(out, "[REDACTED]") {
			t.Fatalf("raw JWT ending %q leaked: %s", suffix, out)
		}
		if out != "token response: [REDACTED]; end" {
			t.Fatalf("JWT boundary text changed: got %q", out)
		}
	}
}

func TestRedact_JWTDoesNotMatchInsideLargerBase64URLIdentifier(t *testing.T) {
	token := "eyJ" + strings.Repeat("a", 16) + "." + strings.Repeat("b", 16) + "." + strings.Repeat("c", 16)
	in := "prefix_" + token + "_suffix"
	if out := Redact(in); out != in {
		t.Fatalf("embedded JWT-like identifier changed: got %q, want %q", out, in)
	}
	for _, match := range Scan(in) {
		if match.RuleID == "jwt" {
			t.Fatalf("embedded JWT-like identifier produced a scan finding: %+v", match)
		}
	}
}

func TestRedact_MultipleJWTsWithSharedDelimiter(t *testing.T) {
	first := "eyJ" + strings.Repeat("a", 16) + "." + strings.Repeat("b", 16) + "." + strings.Repeat("c", 15) + "-"
	second := "eyJ" + strings.Repeat("d", 16) + "." + strings.Repeat("e", 16) + "." + strings.Repeat("f", 15) + "_"
	out := Redact(first + " " + second)
	if strings.Contains(out, first) || strings.Contains(out, second) || strings.Count(out, "[REDACTED]") != 2 {
		t.Fatalf("multiple JWTs were not independently redacted: %q", out)
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

func TestRedactValues_RedactsOpaqueExactValuesAfterGenericRules(t *testing.T) {
	opaqueCredential := "opaque-vendor-credential-value"
	opaqueAccountID := "tenant-account-42"
	recognizable := "ghp_" + strings.Repeat("a", 36)
	in := "credential=" + opaqueCredential + " account=" + opaqueAccountID + " pat=" + recognizable

	out := RedactValues(in, opaqueCredential, opaqueAccountID, opaqueCredential, "")
	for _, value := range []string{opaqueCredential, opaqueAccountID, recognizable} {
		if strings.Contains(out, value) {
			t.Fatal("redacted output retained a protected value")
		}
	}
	if strings.Count(out, "[REDACTED]") != 3 {
		t.Fatalf("redaction marker count = %d, want 3", strings.Count(out, "[REDACTED]"))
	}
}

func TestRedactValues_OverlappingValuesDoNotRewriteRedactionMarkers(t *testing.T) {
	longValue := "opaque-account-identifier"
	shortValue := "account"
	out := RedactValues("id="+longValue+" label="+shortValue, shortValue, longValue)
	if out != "id=[REDACTED] label=[REDACTED]" {
		t.Fatal("overlapping exact values were not redacted atomically")
	}
}

func TestRedactValues_RemovesWholeExactValueContainingRecognizableSecret(t *testing.T) {
	recognizable := "ghp_" + strings.Repeat("b", 36)
	exact := "tenant-prefix-" + recognizable + "-suffix"
	out := RedactValues("credential="+exact, exact)
	if out != "credential=[REDACTED]" {
		t.Fatal("generic redaction prevented whole-value exact redaction")
	}
}

func TestRedactValues_ProtectsExactValueFromContextualGenericPartialMatch(t *testing.T) {
	exact := strings.Repeat("A", 20) + "/opaque-tail"
	out := RedactValues("Authorization: Bearer "+exact, exact)
	if out != "Authorization: Bearer [REDACTED]" {
		t.Fatal("contextual generic rule left part of an exact credential")
	}
}

func TestRedactValues_ShortExactValueCannotCorruptGenericMarker(t *testing.T) {
	recognizable := "ghp_" + strings.Repeat("c", 36)
	out := RedactValues("exact=RED generic="+recognizable, "RED")
	if strings.Count(out, "[REDACTED]") != 2 {
		t.Fatal("short exact value corrupted a generic redaction marker")
	}
}

func TestRedactValuesJSONEscapes(t *testing.T) {
	for _, tc := range []struct {
		name, encoded, secret string
	}{
		{"slash", `opaque\/vendor-token`, "opaque/vendor-token"},
		{"unicode_ascii", `\u006fpaque\u002Fvendor-token`, "opaque/vendor-token"},
		{"unicode_bmp", `opaque-\u4ee4\u724C`, "opaque-令牌"},
		{"unicode_surrogate", `opaque-\uD83D\uDD11`, "opaque-🔑"},
		{"quote_backslash", `opaque-\"quoted\"-\\tail`, "opaque-\"quoted\"-\\tail"},
		{"control_characters", `opaque-\b\f\n\r\t`, "opaque-\b\f\n\r\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := `{"message":"before ` + tc.encoded + ` after","safe":"keep\/this","n":42}`
			got := RedactValues(in, tc.secret)
			var decoded struct {
				Message string `json:"message"`
				Safe    string `json:"safe"`
				N       int    `json:"n"`
			}
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("redaction broke JSON: %q: %v", got, err)
			}
			if decoded.Message != "before [REDACTED] after" || decoded.Safe != "keep/this" || decoded.N != 42 {
				t.Fatalf("decoded redaction = %+v", decoded)
			}
			if !strings.Contains(got, `"safe":"keep\/this"`) {
				t.Fatalf("unrelated JSON string was rewritten: %q", got)
			}
		})
	}
}

func TestRedactValuesJSONEscapesDoNotMatchEncodingSyntax(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{`{"v":"\n"}`, "n"},
		{`{"v":"\u0061"}`, "0061"},
		{`{"access\u005ftoken":"ordinary"}`, "u005f"},
		{`{"v":"keep\/this"}`, "absent-token"},
	} {
		if got := RedactValues(tc.in, tc.secret); got != tc.in {
			t.Fatalf("encoding syntax mistaken for %q: %q", tc.secret, got)
		}
	}
}

func TestRedactValuesJSONPreservesDocumentStructure(t *testing.T) {
	in := " {\n" + `"opaque\/vendor":"echo","safe":1.2300,"safe":9007199254740993,"items":["opaque\u002fvendor",true,null]` + "\n} "
	want := " {\n" + `"[REDACTED]":"echo","safe":1.2300,"safe":9007199254740993,"items":["[REDACTED]",true,null]` + "\n} "
	if got := RedactValues(in, "opaque/vendor"); got != want {
		t.Fatalf("JSON structure or unrelated bytes changed: %q", got)
	}
}

func TestRedactValuesPreservesPlainTextExactMatching(t *testing.T) {
	for _, secret := range []string{`opaque/"vendor"\token`, "opaque-\n-token", `opaque\u0061-token`} {
		if got := RedactValues("upstream echoed "+secret+" after", secret); got != "upstream echoed [REDACTED] after" {
			t.Fatalf("plain-text exact value was not removed: %q", got)
		}
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
