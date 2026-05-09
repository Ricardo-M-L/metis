package auth

// oauth.go is metis's generic OAuth 2.0 (PKCE) client. Used for any
// provider that wants browser-based login instead of paste-an-API-key.
//
// Flow:
//  1. Start localhost callback server on a free port in [7700, 7720)
//  2. Generate PKCE verifier/challenge
//  3. Open user's browser to provider's auth URL
//  4. User authorizes, provider redirects to localhost callback with code
//  5. Exchange code → access token at provider's token endpoint
//  6. Persist token via auth.Set(provider, token)
//
// claude-code's auth uses claude.ai's private OAuth endpoint
// (`utils/auth.ts`). metis ships a generic implementation so any
// provider — GitHub, GitLab, Slack, Discord, custom OIDC — can be
// wired with just a config struct. GitHub is preconfigured below as
// the most common third-party use case (private skill repos / plugin
// marketplaces).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// OAuthProvider describes one OAuth 2.0 endpoint set. Add an entry
// to KnownProviders and `metis auth oauth <name>` will pick it up.
type OAuthProvider struct {
	Name            string
	AuthURL         string   // browser-side auth endpoint
	TokenURL        string   // server-side token exchange endpoint
	ClientID        string   // public client id (no secret — PKCE flow)
	Scopes          []string // OAuth scopes to request
	UsePKCE         bool     // if true, send PKCE challenge (most modern providers)
	ExtraParams     map[string]string
	HeaderTokenType string // typically "Bearer"

	// ManualRedirectURL — if set, enables "manual paste" mode for
	// non-browser environments (SSH, headless, locked-down corp). When
	// the user passes --manual the auth URL is built with this as the
	// redirect_uri instead of localhost; the provider then displays the
	// auth code on a static page and the user pastes it back to the
	// terminal. Mirrors claude-code-sourcemap's MANUAL_REDIRECT_URL.
	ManualRedirectURL string
}

// KnownProviders ships ready-to-use configs for common providers.
// User can override / add more via ~/.metis/oauth-providers.json.
var KnownProviders = map[string]OAuthProvider{
	"github": {
		Name:            "github",
		AuthURL:         "https://github.com/login/oauth/authorize",
		TokenURL:        "https://github.com/login/oauth/access_token",
		ClientID:        "Iv1.b507a08c87ecfe98", // public client id (placeholder for self-host)
		Scopes:          []string{"repo", "read:user"},
		UsePKCE:         true,
		HeaderTokenType: "Bearer",
	},
	// Anthropic Console — the standard developer OAuth path. Endpoints
	// + CLIENT_ID transcribed verbatim from claude-code-sourcemap
	// `restored-src/src/constants/oauth.ts:86-99`. The CONSOLE_*
	// pair (vs CLAUDE_AI_*) is the API-user path; claude.ai
	// subscribers can override via the `anthropic-claudeai` provider
	// below.
	"anthropic": {
		Name:              "anthropic",
		AuthURL:           "https://platform.claude.com/oauth/authorize",
		TokenURL:          "https://platform.claude.com/v1/oauth/token",
		ClientID:          "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:            []string{"org:create_api_key", "user:profile", "user:inference"},
		UsePKCE:           true,
		HeaderTokenType:   "Bearer",
		ManualRedirectURL: "https://platform.claude.com/oauth/code/callback",
	},
	// Anthropic Claude.ai — for users who pay through claude.ai (Pro /
	// Max / Teams). Different authorize URL but same token endpoint
	// + CLIENT_ID. Adds the inference-bearing scope so the returned
	// token can call /v1/messages directly.
	"anthropic-claudeai": {
		Name:              "anthropic-claudeai",
		AuthURL:           "https://claude.com/cai/oauth/authorize",
		TokenURL:          "https://platform.claude.com/v1/oauth/token",
		ClientID:          "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:            []string{"user:inference"},
		UsePKCE:           true,
		HeaderTokenType:   "Bearer",
		ManualRedirectURL: "https://platform.claude.com/oauth/code/callback",
	},
	// gitlab / slack / discord can be added by users via the
	// override file — kept out of defaults to avoid a giant table
	// that few users will touch.
}

// OAuthLogin runs the full browser-based auth dance for the given
// provider name. Blocks until the user authorizes (or 2 min timeout
// expires). On success the access token is persisted to auth.json
// under the provider name.
//
// Returns the access token (also persisted) so callers that want to
// use it immediately don't need a second auth.Get() round-trip.
//
// Equivalent to OAuthLoginOpts(provider, OAuthOptions{}). For manual
// (non-browser) flows, paste-from-stdin, or programmatic paste-via-
// hook, use OAuthLoginOpts directly.
func OAuthLogin(provider string) (string, error) {
	tok, err := OAuthLoginOpts(provider, OAuthOptions{})
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// OAuthOptions tunes the OAuth flow. All fields are optional — the
// zero value matches the original "open browser, listen on localhost"
// flow that OAuthLogin has always done.
type OAuthOptions struct {
	// Manual disables the localhost callback listener. Instead, the
	// auth URL is built with the provider's ManualRedirectURL (which
	// the provider then displays the auth code on a static page) and
	// the caller is responsible for getting the code from the user
	// and feeding it back via PasteCode. Default: false.
	//
	// Required when running under SSH / no-browser environments where
	// localhost isn't reachable from the machine actually running the
	// browser. Mirrors claude-code-sourcemap's "manual flow" from
	// services/oauth/index.ts.
	Manual bool

	// PasteCode supplies the auth code in Manual mode. Called after
	// the auth URL has been displayed to the user. Returning a non-nil
	// error aborts the flow. If nil under Manual=true, OAuthLoginOpts
	// reads a single line from os.Stdin (interactive default).
	PasteCode func(authURL string) (string, error)

	// AuthURLHandler — when set, called instead of openBrowser for
	// the localhost / automatic flow. Lets a UI layer (TUI / SDK
	// control protocol) own the "show this URL" affordance instead
	// of metis spawning `open`/`xdg-open`. nil → default openBrowser.
	AuthURLHandler func(authURL string) error
}

// Token bundles every credential field a modern OAuth response gives
// us. Existing callers using just access_token (via OAuthLogin) keep
// working unchanged — only OAuthLoginOpts callers see the rest.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"` // zero when provider didn't supply expires_in
	Scope        string    `json:"scope,omitempty"`
	TokenType    string    `json:"token_type,omitempty"` // typically "Bearer"
}

// IsExpired reports whether ExpiresAt has passed (with a 60-second
// safety margin so callers don't race the actual cliff).
func (t *Token) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(60 * time.Second).After(t.ExpiresAt)
}

// OAuthLoginOpts is the full-featured login entry point. See
// OAuthOptions for the knobs. On success the token is persisted to
// auth.json (access_token only, for compat with the existing reader)
// AND the rich Token is returned so callers can stash refresh_token
// + expires_at separately if they want.
//
// Future work (not in this commit): persist the rich Token alongside
// auth.json so subsequent runs can refresh transparently.
func OAuthLoginOpts(provider string, opts OAuthOptions) (*Token, error) {
	p, ok := KnownProviders[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %s)",
			provider, strings.Join(knownNames(), ", "))
	}
	if opts.Manual && p.ManualRedirectURL == "" {
		return nil, fmt.Errorf("provider %q has no ManualRedirectURL — manual mode unsupported", provider)
	}

	verifier, challenge := makePKCE()
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	if opts.Manual {
		return runOAuthManual(p, verifier, challenge, state, opts)
	}
	return runOAuthAutomatic(p, verifier, challenge, state, opts)
}

// runOAuthAutomatic — the original "browser + localhost listener"
// flow, factored out so OAuthLoginOpts can dispatch to it cleanly.
func runOAuthAutomatic(p OAuthProvider, verifier, challenge, state string, opts OAuthOptions) (*Token, error) {
	listener, err := pickCallbackPort()
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/callback"

	authURL := buildAuthURL(p, redirectURI, state, challenge, false)
	if opts.AuthURLHandler != nil {
		if err := opts.AuthURLHandler(authURL); err != nil {
			return nil, fmt.Errorf("oauth: AuthURLHandler: %w", err)
		}
	} else if err := openBrowser(authURL); err != nil {
		fmt.Printf("Open this URL to authorize:\n  %s\n", authURL)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := newCallbackServer(state, codeCh, errCh)
	go func() { _ = srv.Serve(listener) }()
	defer srv.Shutdown(context.Background())

	var code string
	select {
	case code = <-codeCh:
	case e := <-errCh:
		return nil, e
	case <-time.After(2 * time.Minute):
		return nil, fmt.Errorf("oauth: user did not authorize within 2 minutes")
	}

	tok, err := exchangeCodeForTokenFull(p, code, redirectURI, verifier)
	if err != nil {
		return nil, err
	}
	if err := Set(p.Name, tok.AccessToken); err != nil {
		return nil, fmt.Errorf("token saved-to-auth: %w", err)
	}
	return tok, nil
}

// runOAuthManual — paste-the-code flow for non-browser environments.
// The auth URL uses ManualRedirectURL so the provider displays the
// code on a static page; we read it from stdin (or the caller's
// PasteCode hook).
func runOAuthManual(p OAuthProvider, verifier, challenge, state string, opts OAuthOptions) (*Token, error) {
	authURL := buildAuthURL(p, p.ManualRedirectURL, state, challenge, true)
	if opts.AuthURLHandler != nil {
		if err := opts.AuthURLHandler(authURL); err != nil {
			return nil, fmt.Errorf("oauth: AuthURLHandler: %w", err)
		}
	} else {
		fmt.Printf("Open this URL in any browser, then paste the code shown:\n\n  %s\n\n", authURL)
	}

	var code string
	if opts.PasteCode != nil {
		c, err := opts.PasteCode(authURL)
		if err != nil {
			return nil, err
		}
		code = strings.TrimSpace(c)
	} else {
		code = strings.TrimSpace(readLineStdin("Paste authorization code: "))
	}
	if code == "" {
		return nil, fmt.Errorf("oauth: no authorization code provided")
	}
	// Some manual-flow servers (Anthropic) display the code in
	// "<code>#<state>" form so the user can verify the state matches.
	// Strip the trailing #state if present.
	if i := strings.Index(code, "#"); i > 0 {
		gotState := code[i+1:]
		code = code[:i]
		if gotState != "" && gotState != state {
			return nil, fmt.Errorf("oauth: state mismatch in pasted code (expected %q, got %q)", state, gotState)
		}
	}

	tok, err := exchangeCodeForTokenFull(p, code, p.ManualRedirectURL, verifier)
	if err != nil {
		return nil, err
	}
	if err := Set(p.Name, tok.AccessToken); err != nil {
		return nil, fmt.Errorf("token saved-to-auth: %w", err)
	}
	return tok, nil
}

func knownNames() []string {
	names := make([]string, 0, len(KnownProviders))
	for k := range KnownProviders {
		names = append(names, k)
	}
	return names
}

func makePKCE() (verifier, challenge string) {
	b := make([]byte, 64)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	_, challenge = makePKCETestSeam(verifier)
	return
}

// makePKCETestSeam derives the S256 challenge from a known verifier.
// Exposed so tests can verify determinism — same verifier in must
// give same challenge out, no matter how many times we call.
func makePKCETestSeam(verifier string) (string, string) {
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pickCallbackPort() (net.Listener, error) {
	for port := 7700; port < 7720; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no free port in 7700-7719")
}

func buildAuthURL(p OAuthProvider, redirectURI, state, challenge string, isManual bool) string {
	q := url.Values{}
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	if p.UsePKCE {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	// Anthropic's manual flow uses `code=true` to signal "show the
	// auth code on a static page instead of redirecting" (see
	// claude-code-sourcemap restored-src/src/services/oauth/client.ts:72).
	// Harmless for providers that ignore it; only Anthropic acts on it
	// today, but the field is generic enough to leave on for any
	// provider that chooses to honour it.
	if isManual {
		q.Set("code", "true")
	}
	for k, v := range p.ExtraParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(p.AuthURL, "?") {
		sep = "&"
	}
	return p.AuthURL + sep + q.Encode()
}

// readLineStdin prints a prompt and reads one line of user input.
// Defined as a var so tests can stub it. ReadLine includes the
// terminating newline, so we trim.
var readLineStdin = func(prompt string) string {
	fmt.Print(prompt)
	var s string
	_, _ = fmt.Scanln(&s)
	return s
}

func newCallbackServer(state string, codeCh chan<- string, errCh chan<- error) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch — possible CSRF, refusing", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth: state parameter mismatch")
			return
		}
		if errMsg := q.Get("error"); errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth: provider returned error: %s", errMsg)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth: provider returned no code")
			return
		}
		codeCh <- code
		// Friendly browser-side completion message — user can close
		// the tab and return to terminal.
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body style="font-family:system-ui;padding:2em">
<h2>✓ Authorized</h2>
<p>You can close this tab and return to your terminal.</p>
</body></html>`))
	})
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// exchangeCodeForToken returns just the access_token for back-compat.
// New callers should use exchangeCodeForTokenFull which preserves
// refresh_token + expires_in for proper rotation support later.
func exchangeCodeForToken(p OAuthProvider, code, redirectURI, verifier string) (string, error) {
	tok, err := exchangeCodeForTokenFull(p, code, redirectURI, verifier)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func exchangeCodeForTokenFull(p OAuthProvider, code, redirectURI, verifier string) (*Token, error) {
	form := url.Values{}
	form.Set("client_id", p.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	if p.UsePKCE {
		form.Set("code_verifier", verifier)
	}

	req, err := http.NewRequest("POST", p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token endpoint %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("token endpoint: bad response: %w", err)
	}
	access, _ := raw["access_token"].(string)
	if access == "" {
		if e, ok := raw["error"].(string); ok {
			return nil, fmt.Errorf("token endpoint: %s", e)
		}
		return nil, fmt.Errorf("token endpoint: no access_token in response")
	}
	tok := &Token{AccessToken: access}
	if v, ok := raw["refresh_token"].(string); ok {
		tok.RefreshToken = v
	}
	if v, ok := raw["scope"].(string); ok {
		tok.Scope = v
	}
	if v, ok := raw["token_type"].(string); ok {
		tok.TokenType = v
	}
	// expires_in is in seconds; convert to absolute time so callers
	// don't have to re-compute against the token-receipt timestamp.
	switch v := raw["expires_in"].(type) {
	case float64:
		if v > 0 {
			tok.ExpiresAt = time.Now().Add(time.Duration(v) * time.Second)
		}
	case string:
		// Some PHP-backed IdPs send expires_in as a string.
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tok.ExpiresAt = time.Now().Add(time.Duration(n) * time.Second)
		}
	}
	return tok, nil
}

// openBrowser launches the user's default browser to the given URL.
// Best-effort — falls back to printing the URL if launch fails.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
}
