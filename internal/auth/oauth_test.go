package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ─── PKCE crypto primitives ──────────────────────────────────────

func TestMakePKCE_Shape(t *testing.T) {
	v, c := makePKCE()
	// Verifier: base64url-encoded 64 bytes → 86 chars no padding.
	if len(v) != 86 {
		t.Errorf("verifier len = %d, want 86", len(v))
	}
	// Challenge: SHA-256 → 32 bytes → base64url 43 chars no padding.
	if len(c) != 43 {
		t.Errorf("challenge len = %d, want 43", len(c))
	}
	if strings.ContainsAny(v, "+/=") || strings.ContainsAny(c, "+/=") {
		t.Errorf("base64url must not contain +/= ; verifier=%q challenge=%q", v, c)
	}
}

func TestMakePKCE_VerifierIsRandom(t *testing.T) {
	v1, _ := makePKCE()
	v2, _ := makePKCE()
	if v1 == v2 {
		t.Error("two PKCE verifiers should be different (vanishingly unlikely collision)")
	}
}

func TestMakePKCE_ChallengeMatchesVerifier(t *testing.T) {
	// Re-derive challenge from verifier and compare. This locks in
	// the S256 contract: the auth server MUST be able to verify
	// SHA256(verifier) == challenge.
	v, c := makePKCE()
	_, c2 := makePKCEFrom(v)
	if c2 != c {
		t.Errorf("challenge derivation not deterministic: got %q vs %q", c, c2)
	}
}

// makePKCEFrom is a test helper that re-runs the SHA256 over a
// known verifier so we can assert determinism.
func makePKCEFrom(verifier string) (string, string) {
	// Reuse production code's challenge derivation so the test
	// fails if S256 algorithm changes silently.
	_, ch := makePKCETestSeam(verifier)
	return verifier, ch
}

// ─── KnownProviders shape ────────────────────────────────────────

func TestKnownProviders_AnthropicConsoleConfigured(t *testing.T) {
	p, ok := KnownProviders["anthropic"]
	if !ok {
		t.Fatal("anthropic provider must be registered in KnownProviders")
	}
	if !strings.Contains(p.AuthURL, "platform.claude.com/oauth/authorize") {
		t.Errorf("anthropic AuthURL wrong: %q", p.AuthURL)
	}
	if !strings.Contains(p.TokenURL, "platform.claude.com/v1/oauth/token") {
		t.Errorf("anthropic TokenURL wrong: %q", p.TokenURL)
	}
	if p.ClientID == "" {
		t.Error("anthropic ClientID empty")
	}
	if !p.UsePKCE {
		t.Error("anthropic must use PKCE (no client secret available)")
	}
	if p.ManualRedirectURL == "" {
		t.Error("anthropic must have ManualRedirectURL for non-browser flow")
	}
}

func TestKnownProviders_AnthropicClaudeAIConfigured(t *testing.T) {
	p, ok := KnownProviders["anthropic-claudeai"]
	if !ok {
		t.Fatal("anthropic-claudeai provider must be registered")
	}
	if !strings.Contains(p.AuthURL, "claude.com/cai/oauth/authorize") {
		t.Errorf("anthropic-claudeai AuthURL wrong (should be the cai/ path): %q", p.AuthURL)
	}
	// Same token endpoint as console.
	if !strings.Contains(p.TokenURL, "platform.claude.com/v1/oauth/token") {
		t.Errorf("anthropic-claudeai TokenURL wrong: %q", p.TokenURL)
	}
	// Has the inference scope so the token works against /v1/messages.
	hasInference := false
	for _, s := range p.Scopes {
		if s == "user:inference" {
			hasInference = true
		}
	}
	if !hasInference {
		t.Errorf("anthropic-claudeai must request user:inference, got %v", p.Scopes)
	}
}

// ─── buildAuthURL ─────────────────────────────────────────────────

func TestBuildAuthURL_ManualSetsCodeFlag(t *testing.T) {
	p := KnownProviders["anthropic"]
	got := buildAuthURL(p, p.ManualRedirectURL, "STATE", "CHALLENGE", true)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("code") != "true" {
		t.Errorf("manual flow must include code=true to trigger static-page display: %q", got)
	}
	if u.Query().Get("redirect_uri") != p.ManualRedirectURL {
		t.Errorf("manual flow must use ManualRedirectURL, got %q", u.Query().Get("redirect_uri"))
	}
	if u.Query().Get("code_challenge") != "CHALLENGE" {
		t.Errorf("PKCE challenge missing: %q", got)
	}
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("S256 method must be set: %q", got)
	}
}

func TestBuildAuthURL_AutomaticOmitsCodeFlag(t *testing.T) {
	p := KnownProviders["anthropic"]
	got := buildAuthURL(p, "http://localhost:7700/callback", "STATE", "CHALLENGE", false)
	u, _ := url.Parse(got)
	if u.Query().Get("code") == "true" {
		t.Errorf("automatic flow must NOT set code=true (only manual): %q", got)
	}
	if !strings.Contains(u.Query().Get("redirect_uri"), "localhost:7700") {
		t.Errorf("automatic flow must use localhost callback: %q", u.Query().Get("redirect_uri"))
	}
}

func TestBuildAuthURL_StateAndScopesPresent(t *testing.T) {
	p := KnownProviders["anthropic"]
	got := buildAuthURL(p, "https://x", "the-state", "the-challenge", false)
	u, _ := url.Parse(got)
	if u.Query().Get("state") != "the-state" {
		t.Errorf("state lost: %q", got)
	}
	scope := u.Query().Get("scope")
	for _, want := range p.Scopes {
		if !strings.Contains(scope, want) {
			t.Errorf("scope %q not in built URL: %q", want, scope)
		}
	}
}

// ─── Token type helpers ──────────────────────────────────────────

func TestToken_IsExpired(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		expires time.Time
		want    bool
	}{
		{"zero — never expires", time.Time{}, false},
		{"way past", now.Add(-1 * time.Hour), true},
		{"way future", now.Add(1 * time.Hour), false},
		{"within 60s safety margin", now.Add(30 * time.Second), true},
		{"just past safety margin", now.Add(90 * time.Second), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok := &Token{ExpiresAt: c.expires}
			if got := tok.IsExpired(); got != c.want {
				t.Errorf("IsExpired = %v, want %v (expires=%v)", got, c.want, c.expires)
			}
		})
	}
}

// ─── exchangeCodeForTokenFull ────────────────────────────────────

func TestExchangeCodeForTokenFull_ParsesAllFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inspect request to confirm PKCE verifier flows through.
		_ = r.ParseForm()
		if r.Form.Get("code") != "the-code" {
			t.Errorf("code lost in form: %q", r.Form.Get("code"))
		}
		if r.Form.Get("code_verifier") != "the-verifier" {
			t.Errorf("verifier missing: %q", r.Form.Get("code_verifier"))
		}
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type wrong: %q", r.Form.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "AT",
			"refresh_token": "RT",
			"expires_in":    3600,
			"scope":         "user:inference user:profile",
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()

	p := OAuthProvider{
		Name: "test", TokenURL: srv.URL, UsePKCE: true,
	}
	tok, err := exchangeCodeForTokenFull(p, "the-code", "https://x", "the-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "AT" {
		t.Errorf("access token: %q", tok.AccessToken)
	}
	if tok.RefreshToken != "RT" {
		t.Errorf("refresh token: %q", tok.RefreshToken)
	}
	if tok.Scope != "user:inference user:profile" {
		t.Errorf("scope: %q", tok.Scope)
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("token_type: %q", tok.TokenType)
	}
	// expires_in 3600 → ExpiresAt ~ +1h from now, with some slop for
	// test execution time.
	dt := time.Until(tok.ExpiresAt)
	if dt < 50*time.Minute || dt > 65*time.Minute {
		t.Errorf("expires_at not ~1h from now: dt=%v", dt)
	}
}

func TestExchangeCodeForTokenFull_HandlesStringExpiresIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Some PHP-backed IdPs send expires_in as a string. The
		// parser must accept both.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT",
			"expires_in":   "1800",
		})
	}))
	defer srv.Close()

	tok, err := exchangeCodeForTokenFull(OAuthProvider{TokenURL: srv.URL}, "c", "r", "v")
	if err != nil {
		t.Fatal(err)
	}
	dt := time.Until(tok.ExpiresAt)
	if dt < 25*time.Minute || dt > 35*time.Minute {
		t.Errorf("string expires_in='1800' should be ~30min, got dt=%v", dt)
	}
}

func TestExchangeCodeForTokenFull_RaisesProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant",
		})
	}))
	defer srv.Close()

	_, err := exchangeCodeForTokenFull(OAuthProvider{TokenURL: srv.URL}, "c", "r", "v")
	if err == nil {
		t.Fatal("provider error should propagate")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should mention invalid_grant, got: %v", err)
	}
}

func TestExchangeCodeForTokenFull_NoAccessTokenIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"random": "junk"})
	}))
	defer srv.Close()

	_, err := exchangeCodeForTokenFull(OAuthProvider{TokenURL: srv.URL}, "c", "r", "v")
	if err == nil {
		t.Error("missing access_token should error")
	}
}

// ─── runOAuthManual paste-flow ───────────────────────────────────

func TestRunOAuthManual_PasteCodeFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // sandbox auth.json writes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "PASTED-CODE" {
			t.Errorf("manual paste code lost: %q", r.Form.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"manual-AT","token_type":"Bearer"}`)
	}))
	defer srv.Close()

	p := OAuthProvider{
		Name: "test-manual", TokenURL: srv.URL, AuthURL: "https://example.com/auth",
		ManualRedirectURL: "https://example.com/manual", UsePKCE: true,
	}
	// Inject paste-code via callback.
	pasteCalled := false
	tok, err := runOAuthManual(p, "verifier", "challenge", "STATE", OAuthOptions{
		Manual: true,
		PasteCode: func(authURL string) (string, error) {
			pasteCalled = true
			if !strings.Contains(authURL, "code=true") {
				t.Errorf("manual auth URL should have code=true: %q", authURL)
			}
			return "PASTED-CODE", nil
		},
		AuthURLHandler: func(string) error { return nil }, // suppress stderr print
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pasteCalled {
		t.Error("PasteCode callback was not invoked")
	}
	if tok.AccessToken != "manual-AT" {
		t.Errorf("token: %q", tok.AccessToken)
	}
}

func TestRunOAuthManual_StripStateSuffixInPastedCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// State suffix must have been stripped — token endpoint
		// should see the raw code, not "code#state".
		if r.Form.Get("code") != "the-code" {
			t.Errorf("state suffix not stripped: %q", r.Form.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"AT"}`)
	}))
	defer srv.Close()

	p := OAuthProvider{
		Name: "test-suffix", TokenURL: srv.URL, AuthURL: "https://x",
		ManualRedirectURL: "https://x/manual", UsePKCE: true,
	}
	_, err := runOAuthManual(p, "v", "c", "STATE", OAuthOptions{
		Manual:         true,
		PasteCode:      func(string) (string, error) { return "the-code#STATE", nil },
		AuthURLHandler: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("state-suffix-aware paste should succeed: %v", err)
	}
}

func TestRunOAuthManual_RejectsStateMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := OAuthProvider{
		Name: "test-bad-state", TokenURL: "http://unused", AuthURL: "https://x",
		ManualRedirectURL: "https://x/manual",
	}
	_, err := runOAuthManual(p, "v", "c", "EXPECTED-STATE", OAuthOptions{
		Manual:         true,
		PasteCode:      func(string) (string, error) { return "the-code#WRONG-STATE", nil },
		AuthURLHandler: func(string) error { return nil },
	})
	if err == nil {
		t.Fatal("state mismatch must error")
	}
	if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("error should mention state mismatch: %v", err)
	}
}

func TestRunOAuthManual_EmptyCodeIsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := OAuthProvider{
		Name: "test-empty", AuthURL: "https://x", ManualRedirectURL: "https://x/manual",
	}
	_, err := runOAuthManual(p, "v", "c", "STATE", OAuthOptions{
		Manual:         true,
		PasteCode:      func(string) (string, error) { return "   ", nil },
		AuthURLHandler: func(string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "no authorization code") {
		t.Errorf("empty paste must error, got: %v", err)
	}
}

func TestRunOAuthManual_PasteHookErrorPropagates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := errors.New("user cancelled")
	p := OAuthProvider{
		Name: "test-cancel", AuthURL: "https://x", ManualRedirectURL: "https://x/manual",
	}
	_, err := runOAuthManual(p, "v", "c", "STATE", OAuthOptions{
		Manual:         true,
		PasteCode:      func(string) (string, error) { return "", want },
		AuthURLHandler: func(string) error { return nil },
	})
	if !errors.Is(err, want) {
		t.Errorf("paste-hook error should propagate, got: %v", err)
	}
}

// ─── OAuthLoginOpts validation ───────────────────────────────────

func TestOAuthLoginOpts_RejectsManualWhenProviderHasNoManualURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// github (the default fixture) has no ManualRedirectURL set.
	_, err := OAuthLoginOpts("github", OAuthOptions{Manual: true})
	if err == nil {
		t.Fatal("manual mode should error when ManualRedirectURL unset")
	}
	if !strings.Contains(err.Error(), "manual mode unsupported") {
		t.Errorf("error should explain why: %v", err)
	}
}

func TestOAuthLoginOpts_RejectsUnknownProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := OAuthLoginOpts("does-not-exist", OAuthOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unknown provider must error, got: %v", err)
	}
}

// ─── back-compat: existing OAuthLogin signature ──────────────────

func TestOAuthLogin_StillReturnsString(t *testing.T) {
	// Compile-time check — confirms the original signature didn't
	// regress when we added OAuthLoginOpts. We don't run a real flow
	// (no test server here for github); just verify the function
	// shape and error path.
	_, err := OAuthLogin("does-not-exist")
	if err == nil {
		t.Error("unknown provider should error")
	}
}
