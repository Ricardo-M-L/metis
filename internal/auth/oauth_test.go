package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

func TestKnownProviders_AnthropicSubscriberConfigured(t *testing.T) {
	p, ok := KnownProviders["anthropic"]
	if !ok {
		t.Fatal("anthropic provider must be registered in KnownProviders")
	}
	if p.AuthURL != "https://claude.ai/oauth/authorize" {
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
	if p.CallbackAddress != "127.0.0.1:53692" || p.CallbackRedirectURL != "http://localhost:53692/callback" || p.CallbackPath != "/callback" {
		t.Errorf("anthropic fixed callback mismatch: %+v", p)
	}
	if !p.TokenRequestJSON || !p.IncludeStateInTokenRequest {
		t.Error("anthropic token exchange must use JSON and include OAuth state")
	}
	// Has the inference scope so the token works against /v1/messages.
	hasInference := false
	for _, s := range p.Scopes {
		if s == "user:inference" {
			hasInference = true
		}
	}
	if !hasInference {
		t.Errorf("anthropic must request user:inference, got %v", p.Scopes)
	}
}

func TestKnownProviders_OpenAICodexConfigured(t *testing.T) {
	p, ok := KnownProviders["openai-codex"]
	if !ok {
		t.Fatal("openai-codex provider must be registered")
	}
	if p.AuthURL != "https://auth.openai.com/oauth/authorize" || p.TokenURL != "https://auth.openai.com/oauth/token" {
		t.Fatalf("openai-codex endpoints mismatch: %+v", p)
	}
	if p.ClientID != "app_EMoamEEZ73f0CkXaXp7hrann" || !p.UsePKCE {
		t.Fatal("openai-codex must use its public native-app client id with PKCE")
	}
	if p.CallbackAddress != "127.0.0.1:1455" || p.CallbackRedirectURL != "http://localhost:1455/auth/callback" || p.CallbackPath != "/auth/callback" {
		t.Errorf("openai-codex fixed callback mismatch: %+v", p)
	}
	if p.TokenRequestJSON {
		t.Error("OpenAI token exchange must use form encoding")
	}
}

func TestKnownProviders_DeprecatedAnthropicAliasRemainsCanonical(t *testing.T) {
	p, ok := KnownProviders["anthropic-claudeai"]
	if !ok {
		t.Fatal("deprecated anthropic-claudeai alias must remain available")
	}
	if p.Name != "anthropic" || canonicalOAuthProviderID("anthropic-claudeai") != "anthropic" {
		t.Fatalf("deprecated alias is not canonical: %+v", p)
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

func TestBuildAuthURL_AutomaticOmitsCodeFlagForGenericProvider(t *testing.T) {
	p := OAuthProvider{AuthURL: "https://issuer.example.test/authorize", UsePKCE: true}
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
	t.Setenv("METIS_HOME", t.TempDir()) // sandbox auth.json writes
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
			if strings.Contains(authURL, "code=true") {
				t.Errorf("generic providers must not receive Anthropic's code=true parameter: %q", authURL)
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
	stored, err := GetOAuth(p.Name)
	if err != nil {
		t.Fatalf("read default OAuth persistence: %v", err)
	}
	if stored == nil || stored.AccessToken != "manual-AT" {
		t.Fatalf("default OAuth persistence = %+v, want access token manual-AT", stored)
	}
	if key, err := Get(p.Name); err != nil || key != "" {
		t.Fatalf("OAuth token was also exposed as an API key: key=%q err=%v", key, err)
	}
}

func TestRunOAuthManual_SkipPersistReturnsTokenWithoutWritingAuthJSON(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"mcp-only","refresh_token":"refresh-only"}`)
	}))
	defer srv.Close()

	p := OAuthProvider{
		Name: "mcp:test", TokenURL: srv.URL, AuthURL: "https://example.com/auth",
		ManualRedirectURL: "https://example.com/manual", UsePKCE: true,
	}
	tok, err := runOAuthManual(p, "verifier", "challenge", "STATE", OAuthOptions{
		Manual:      true,
		SkipPersist: true,
		PasteCode:   func(string) (string, error) { return "PASTED-CODE", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "mcp-only" || tok.RefreshToken != "refresh-only" {
		t.Fatalf("returned rich token = %+v", tok)
	}
	if _, err := os.Stat(Path()); !os.IsNotExist(err) {
		t.Fatalf("SkipPersist wrote auth.json: stat error = %v", err)
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

func TestRunOAuthManual_KnownFixedCallbackAcceptsBareCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if got := r.Form.Get("code"); got != "code-without-state" {
			t.Errorf("code = %q", got)
		}
		if got := r.Form.Get("state"); got != "" {
			t.Errorf("OpenAI token exchange unexpectedly included state %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"AT"}`)
	}))
	defer server.Close()

	p := KnownProviders["openai-codex"]
	p.TokenURL = server.URL
	token, err := runOAuthManual(p, "verifier", "challenge", "EXPECTED-STATE", OAuthOptions{
		Manual:         true,
		PasteCode:      func(string) (string, error) { return "code-without-state", nil },
		AuthURLHandler: func(string) error { return nil },
		SkipPersist:    true,
	})
	if err != nil {
		t.Fatalf("Pi-compatible bare authorization code failed: %v", err)
	}
	if token == nil || token.AccessToken != "AT" {
		t.Fatalf("token = %#v", token)
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

func TestRunOAuthAutomaticContext_CancelStopsCallbackWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opened := make(chan struct{}, 1)
	p := OAuthProvider{
		Name: "cancel-test", AuthURL: "https://example.test/authorize",
		TokenURL: "https://example.test/token", ClientID: "client", UsePKCE: true,
	}

	done := make(chan error, 1)
	go func() {
		_, err := runOAuthAutomaticContext(ctx, p, "verifier", "challenge", "state", OAuthOptions{
			AuthURLHandler: func(string) error {
				opened <- struct{}{}
				return nil
			},
		})
		done <- err
	}()

	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("OAuth flow did not reach callback wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled OAuth error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled OAuth callback wait did not return")
	}
}

func TestRunOAuthAutomaticBusyPortFallsBackToPaste(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("code"); got != "pasted-code" {
			t.Errorf("code = %q, want pasted-code", got)
		}
		_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
	}))
	defer tokenServer.Close()

	callbackURL := "http://" + occupied.Addr().String() + "/callback"
	p := OAuthProvider{
		Name: "busy-port", AuthURL: "https://issuer.example.test/authorize",
		TokenURL: tokenServer.URL, ClientID: "client", UsePKCE: true,
		CallbackAddress: occupied.Addr().String(), CallbackRedirectURL: callbackURL,
		CallbackPath: "/callback", ManualRedirectURL: callbackURL,
	}
	tok, err := runOAuthAutomaticContext(context.Background(), p, "verifier", "challenge", "state", OAuthOptions{
		SkipPersist:    true,
		AuthURLHandler: func(string) error { return nil },
		PasteCodeContext: func(context.Context, string) (string, error) {
			return "pasted-code#state", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access" || tok.RefreshToken != "refresh" {
		t.Fatalf("token = %#v", tok)
	}
}

func TestRunOAuthAutomaticAcceptsPastedRedirectAlongsideCallback(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"pasted-access"}`)
	}))
	defer tokenServer.Close()
	p := OAuthProvider{
		Name: "paste-race", AuthURL: "https://issuer.example.test/authorize",
		TokenURL: tokenServer.URL, ClientID: "client", UsePKCE: true,
	}
	tok, err := runOAuthAutomaticContext(context.Background(), p, "verifier", "challenge", "state", OAuthOptions{
		SkipPersist:    true,
		AuthURLHandler: func(string) error { return nil },
		PasteCodeContext: func(context.Context, string) (string, error) {
			return "pasted-code#state", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "pasted-access" {
		t.Fatalf("access token = %q, want pasted-access", tok.AccessToken)
	}
}

func TestRunOAuthAutomaticCallbackCancelsAndJoinsPasteReader(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"callback-access"}`)
	}))
	defer tokenServer.Close()
	p := OAuthProvider{
		Name: "callback-race", AuthURL: "https://issuer.example.test/authorize",
		TokenURL: tokenServer.URL, ClientID: "client", UsePKCE: true,
	}
	authURLCh := make(chan string, 1)
	pasteStarted := make(chan struct{})
	pasteStopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, runErr := runOAuthAutomaticContext(context.Background(), p, "verifier", "challenge", "state", OAuthOptions{
			SkipPersist: true,
			AuthURLHandler: func(authURL string) error {
				authURLCh <- authURL
				return nil
			},
			PasteCodeContext: func(ctx context.Context, _ string) (string, error) {
				close(pasteStarted)
				<-ctx.Done()
				close(pasteStopped)
				return "", ctx.Err()
			},
		})
		done <- runErr
	}()

	select {
	case <-pasteStarted:
	case <-time.After(time.Second):
		t.Fatal("automatic paste reader did not start")
	}
	authURL := <-authURLCh
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	callbackURL := parsed.Query().Get("redirect_uri")
	callback, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	query := callback.Query()
	query.Set("code", "callback-code")
	query.Set("state", "state")
	callback.RawQuery = query.Encode()
	resp, err := http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("OAuth callback did not complete")
	}
	select {
	case <-pasteStopped:
	case <-time.After(time.Second):
		t.Fatal("callback winner left the paste reader running")
	}
}

func TestExchangeCodeForTokenFullContext_CancelStopsHTTPRequest(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requestStarted <- struct{}{}
		<-releaseHandler
	}))
	defer srv.Close()
	defer close(releaseHandler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exchangeCodeForTokenFullContext(ctx, OAuthProvider{TokenURL: srv.URL}, "code", "redirect", "verifier")
		done <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("token exchange request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled token exchange error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled token exchange did not return")
	}
}
