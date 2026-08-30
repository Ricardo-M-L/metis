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
//  6. Persist token via auth.Set(provider, token), unless the caller owns a
//     separate credential store and opts out with OAuthOptions.SkipPersist
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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// oauthHTTPClient bounds the code→token exchange and refresh calls. The
// browser-authorization step has its own 2-minute select timeout, but the
// token exchange used http.DefaultClient (NO timeout): a wedged or malicious
// token endpoint would hang the CLI forever after the user had already
// authorized. 30s is generous for an OAuth round-trip.
var (
	errOAuthCrossOriginRedirect = errors.New("oauth: cross-origin redirect rejected")
	errOAuthTooManyRedirects    = errors.New("oauth: stopped after 10 redirects")
	oauthHTTPClient             = &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: checkOAuthSameOriginRedirect,
	}
)

const (
	oauthAuthorizationTimeout = 2 * time.Minute
	oauthMaxJSONResponseBytes = 1 << 20
	oauthCallbackShutdownWait = time.Second
	oauthMaxProviderCodeBytes = 64
)

// oauthEndpointError is deliberately a closed, model-safe error boundary for
// token endpoints. OAuth servers are untrusted and sometimes echo submitted
// grant credentials in `error`, `error_description`, response bodies, redirect
// locations, or transport failures. Only a stable operation, HTTP status, and
// a tightly bounded error code may cross this boundary.
type oauthEndpointError struct {
	operation  string
	statusCode int
	code       string
}

func (e *oauthEndpointError) Error() string {
	if e == nil {
		return "oauth: token request failed"
	}
	detail := ""
	if e.statusCode > 0 {
		detail = fmt.Sprintf("HTTP %d", e.statusCode)
	}
	if e.code != "" {
		if detail != "" {
			detail += ", "
		}
		detail += "code " + e.code
	}
	if detail == "" {
		return "oauth: " + e.operation + " failed"
	}
	return "oauth: " + e.operation + " failed (" + detail + ")"
}

// OAuthStatusCode and OAuthErrorCode let higher layers retain the bounded
// diagnostic fields without depending on this package's private error type.
func (e *oauthEndpointError) OAuthStatusCode() int   { return e.statusCode }
func (e *oauthEndpointError) OAuthErrorCode() string { return e.code }

// OAuthProvider describes one OAuth 2.0 endpoint set. Add an entry
// to KnownProviders and `metis auth oauth <name>` will pick it up.
type OAuthProvider struct {
	Name            string
	AuthURL         string   // browser-side auth endpoint
	TokenURL        string   // server-side token exchange endpoint
	ClientID        string   // public client id (no secret — PKCE flow)
	Scopes          []string // OAuth scopes to request
	UsePKCE         bool     // if true, send PKCE challenge (most modern providers)
	ResourceURL     string   // RFC 8707 resource indicator / intended token audience
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
	return OAuthLoginContext(context.Background(), provider)
}

// OAuthLoginContext is the cancellable form of OAuthLogin. Cancellation closes
// the localhost callback listener and aborts the token HTTP exchange.
func OAuthLoginContext(ctx context.Context, provider string) (string, error) {
	tok, err := OAuthLoginOptsContext(ctx, provider, OAuthOptions{})
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// OAuthOptions tunes the OAuth flow. All fields are optional — the
// zero value matches the original "open browser, listen on localhost"
// flow that OAuthLogin has always done.
type OAuthOptions struct {
	// SkipPersist returns the rich token without copying its access token into
	// auth.json. This is intended for callers with their own credential store
	// (for example MCP OAuth's mcp-oauth.json). The default is false so all
	// existing provider-login callers retain the original persistence behavior.
	SkipPersist bool

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

	// PasteCodeContext is the cancellable form preferred by UI clients. The
	// context is canceled when the login screen/session is closed. PasteCode is
	// retained for source compatibility; the outer login call can stop waiting
	// for it, but a legacy callback that ignores its own lifecycle may continue
	// running in the background.
	PasteCodeContext func(context.Context, string) (string, error)

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

// OAuthLoginOpts is the full-featured login entry point. See OAuthOptions for
// the knobs. By default, success persists access_token to auth.json for
// compatibility with existing provider readers and returns the rich Token.
// Callers with a dedicated credential store can set SkipPersist and persist
// the returned access + refresh + expiry fields themselves.
//
// Future work (not in this commit): persist the rich Token alongside
// auth.json so subsequent runs can refresh transparently.
func OAuthLoginOpts(provider string, opts OAuthOptions) (*Token, error) {
	return OAuthLoginOptsContext(context.Background(), provider, opts)
}

// OAuthLoginOptsContext is OAuthLoginOpts with caller-owned cancellation.
// UI callers should always use this form so closing a screen or application
// tears down both the callback listener and any in-flight HTTP request.
func OAuthLoginOptsContext(ctx context.Context, provider string, opts OAuthOptions) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
		return runOAuthManualContext(ctx, p, verifier, challenge, state, opts)
	}
	return runOAuthAutomaticContext(ctx, p, verifier, challenge, state, opts)
}

// runOAuthAutomatic — the original "browser + localhost listener"
// flow, factored out so OAuthLoginOpts can dispatch to it cleanly.
func runOAuthAutomatic(p OAuthProvider, verifier, challenge, state string, opts OAuthOptions) (*Token, error) {
	return runOAuthAutomaticContext(context.Background(), p, verifier, challenge, state, opts)
}

func runOAuthAutomaticContext(ctx context.Context, p OAuthProvider, verifier, challenge, state string, opts OAuthOptions) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	resultCh := make(chan oauthCallbackResult, 1)
	srv := newCallbackServer(state, resultCh)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case resultCh <- oauthCallbackResult{err: fmt.Errorf("oauth: callback server: %w", err)}:
			default:
			}
		}
	}()
	defer shutdownOAuthCallbackServer(srv, serveDone)

	timer := time.NewTimer(oauthAuthorizationTimeout)
	defer timer.Stop()
	var code string
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}
		code = result.code
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("oauth: user did not authorize within 2 minutes")
	}

	tok, err := exchangeCodeForTokenFullContext(ctx, p, code, redirectURI, verifier)
	if err != nil {
		return nil, err
	}
	if !opts.SkipPersist {
		if err := Set(p.Name, tok.AccessToken); err != nil {
			return nil, fmt.Errorf("token saved-to-auth: %w", err)
		}
	}
	return tok, nil
}

// runOAuthManual — paste-the-code flow for non-browser environments.
// The auth URL uses ManualRedirectURL so the provider displays the
// code on a static page; we read it from stdin (or the caller's
// PasteCode hook).
func runOAuthManual(p OAuthProvider, verifier, challenge, state string, opts OAuthOptions) (*Token, error) {
	return runOAuthManualContext(context.Background(), p, verifier, challenge, state, opts)
}

func runOAuthManualContext(ctx context.Context, p OAuthProvider, verifier, challenge, state string, opts OAuthOptions) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authURL := buildAuthURL(p, p.ManualRedirectURL, state, challenge, true)
	if opts.AuthURLHandler != nil {
		if err := opts.AuthURLHandler(authURL); err != nil {
			return nil, fmt.Errorf("oauth: AuthURLHandler: %w", err)
		}
	} else {
		fmt.Printf("Open this URL in any browser, then paste the code shown:\n\n  %s\n\n", authURL)
	}

	code, err := waitForManualCode(ctx, authURL, opts)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("oauth: no authorization code provided")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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

	tok, err := exchangeCodeForTokenFullContext(ctx, p, code, p.ManualRedirectURL, verifier)
	if err != nil {
		return nil, err
	}
	if !opts.SkipPersist {
		if err := Set(p.Name, tok.AccessToken); err != nil {
			return nil, fmt.Errorf("token saved-to-auth: %w", err)
		}
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
	for k, v := range p.ExtraParams {
		q.Set(k, v)
	}
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	if p.ResourceURL != "" {
		q.Set("resource", p.ResourceURL)
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

type oauthCallbackResult struct {
	code string
	err  error
}

func newCallbackServer(state string, resultCh chan<- oauthCallbackResult) *http.Server {
	mux := http.NewServeMux()
	var publishOnce sync.Once
	publish := func(result oauthCallbackResult) {
		publishOnce.Do(func() {
			select {
			case resultCh <- result:
			default:
			}
		})
	}
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch — possible CSRF, refusing", http.StatusBadRequest)
			publish(oauthCallbackResult{err: fmt.Errorf("oauth: state parameter mismatch")})
			return
		}
		if errMsg := q.Get("error"); errMsg != "" {
			// The provider controls this query value. Keep arbitrary text out of
			// both the browser response and the caller-visible error; retain only a
			// short OAuth-shaped code when one was supplied.
			code := sanitizeOAuthProviderCode(errMsg, nil)
			http.Error(w, "OAuth authorization failed", http.StatusBadRequest)
			if code == "" {
				publish(oauthCallbackResult{err: errors.New("oauth: provider authorization failed")})
			} else {
				publish(oauthCallbackResult{err: fmt.Errorf("oauth: provider authorization failed (code %s)", code)})
			}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			publish(oauthCallbackResult{err: fmt.Errorf("oauth: provider returned no code")})
			return
		}
		// Friendly browser-side completion message — user can close
		// the tab and return to terminal.
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body style="font-family:system-ui;padding:2em">
<h2>✓ Authorized</h2>
<p>You can close this tab and return to your terminal.</p>
</body></html>`))
		publish(oauthCallbackResult{code: code})
	})
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func shutdownOAuthCallbackServer(server *http.Server, serveDone <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), oauthCallbackShutdownWait)
	defer cancel()
	_ = server.Shutdown(ctx)
	select {
	case <-serveDone:
	case <-ctx.Done():
		_ = server.Close()
	}
}

type manualCodeResult struct {
	code string
	err  error
}

func waitForManualCode(ctx context.Context, authURL string, opts OAuthOptions) (string, error) {
	resultCh := make(chan manualCodeResult, 1)
	go func() {
		var code string
		var err error
		switch {
		case opts.PasteCodeContext != nil:
			code, err = opts.PasteCodeContext(ctx, authURL)
		case opts.PasteCode != nil:
			code, err = opts.PasteCode(authURL)
		default:
			code = readLineStdin("Paste authorization code: ")
		}
		resultCh <- manualCodeResult{code: code, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-resultCh:
		return result.code, result.err
	}
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
	return exchangeCodeForTokenFullContext(context.Background(), p, code, redirectURI, verifier)
}

func exchangeCodeForTokenFullContext(ctx context.Context, p OAuthProvider, code, redirectURI, verifier string) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const operation = "token exchange"
	form := url.Values{}
	form.Set("client_id", p.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	if p.ResourceURL != "" {
		form.Set("resource", p.ResourceURL)
	}
	if p.UsePKCE {
		form.Set("code_verifier", verifier)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, newOAuthEndpointError(operation, 0, "invalid_request", code, verifier)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return nil, safeOAuthRequestError(ctx, operation, err, code, verifier)
	}
	defer resp.Body.Close()
	return decodeOAuthTokenResponse(resp, operation, []string{code, verifier})
}

// tokenFromRawForEndpoint decodes a token response while retaining the grant
// secrets needed to reject provider error fields that echo those credentials.
func tokenFromRawForEndpoint(raw map[string]any, operation string, statusCode int, secrets []string) (*Token, error) {
	access, _ := raw["access_token"].(string)
	if access == "" {
		code, _ := raw["error"].(string)
		code = sanitizeOAuthProviderCode(code, secrets)
		if code == "" {
			code = "missing_access_token"
		}
		return nil, newOAuthEndpointError(operation, statusCode, code, secrets...)
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
	switch v := raw["expires_in"].(type) {
	case float64:
		if v > 0 {
			tok.ExpiresAt = time.Now().Add(time.Duration(v) * time.Second)
		}
	case string:
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tok.ExpiresAt = time.Now().Add(time.Duration(n) * time.Second)
		}
	}
	return tok, nil
}

// OAuthLoginWithProvider runs the PKCE login flow against an explicitly
// supplied provider config (e.g. one discovered from an MCP server's
// .well-known metadata) rather than a KnownProviders name. Returns the
// rich Token (access + refresh + expiry).
func OAuthLoginWithProvider(p OAuthProvider, opts OAuthOptions) (*Token, error) {
	return OAuthLoginWithProviderContext(context.Background(), p, opts)
}

// OAuthLoginWithProviderContext is the cancellable form used by MCP OAuth,
// whose provider metadata is discovered dynamically rather than by name.
func OAuthLoginWithProviderContext(ctx context.Context, p OAuthProvider, opts OAuthOptions) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	verifier, challenge := makePKCE()
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	if opts.Manual {
		if p.ManualRedirectURL == "" {
			return nil, fmt.Errorf("manual mode requires ManualRedirectURL")
		}
		return runOAuthManualContext(ctx, p, verifier, challenge, state, opts)
	}
	return runOAuthAutomaticContext(ctx, p, verifier, challenge, state, opts)
}

// RefreshToken exchanges a refresh_token for a fresh access token at the
// provider's token endpoint. Used to renew an expired MCP OAuth token
// without a new browser round-trip.
func RefreshToken(p OAuthProvider, refreshToken string) (*Token, error) {
	return RefreshTokenContext(context.Background(), p, refreshToken)
}

// RefreshTokenContext aborts a refresh request when its owning MCP launch or
// application context is canceled.
func RefreshTokenContext(ctx context.Context, p OAuthProvider, refreshToken string) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const operation = "token refresh"
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if p.ClientID != "" {
		form.Set("client_id", p.ClientID)
	}
	if len(p.Scopes) > 0 {
		form.Set("scope", strings.Join(p.Scopes, " "))
	}
	if p.ResourceURL != "" {
		form.Set("resource", p.ResourceURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, newOAuthEndpointError(operation, 0, "invalid_request", refreshToken)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return nil, safeOAuthRequestError(ctx, operation, err, refreshToken)
	}
	defer resp.Body.Close()
	tok, err := decodeOAuthTokenResponse(resp, operation, []string{refreshToken})
	if err != nil {
		return nil, err
	}
	// Some providers omit refresh_token on refresh — keep the old one.
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func decodeOAuthTokenResponse(resp *http.Response, operation string, secrets []string) (*Token, error) {
	if resp == nil {
		return nil, newOAuthEndpointError(operation, 0, "invalid_response", secrets...)
	}
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, oauthMaxJSONResponseBytes+1))
	if err != nil {
		return nil, newOAuthEndpointError(operation, resp.StatusCode, "response_read_failed", secrets...)
	}
	if len(rawBody) > oauthMaxJSONResponseBytes {
		return nil, newOAuthEndpointError(operation, resp.StatusCode, "response_too_large", secrets...)
	}

	var raw map[string]any
	decodeErr := json.Unmarshal(rawBody, &raw)
	if resp.StatusCode != http.StatusOK {
		code := ""
		if decodeErr == nil {
			code, _ = raw["error"].(string)
		}
		return nil, newOAuthEndpointError(operation, resp.StatusCode, code, secrets...)
	}
	if decodeErr != nil {
		return nil, newOAuthEndpointError(operation, resp.StatusCode, "invalid_response", secrets...)
	}
	return tokenFromRawForEndpoint(raw, operation, resp.StatusCode, secrets)
}

func safeOAuthRequestError(ctx context.Context, operation string, err error, secrets ...string) error {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	code := "request_failed"
	switch {
	case errors.Is(err, errOAuthCrossOriginRedirect):
		code = "cross_origin_redirect"
	case errors.Is(err, errOAuthTooManyRedirects):
		code = "too_many_redirects"
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			code = "request_timeout"
		}
	}
	return newOAuthEndpointError(operation, 0, code, secrets...)
}

func newOAuthEndpointError(operation string, statusCode int, rawCode string, secrets ...string) error {
	code := sanitizeOAuthProviderCode(rawCode, secrets)
	return &oauthEndpointError{operation: operation, statusCode: statusCode, code: code}
}

func sanitizeOAuthProviderCode(raw string, secrets []string) string {
	code := strings.TrimSpace(raw)
	if code == "" || len(code) > oauthMaxProviderCodeBytes || containsOAuthSecret(code, secrets) {
		return ""
	}
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '~' {
			continue
		}
		return ""
	}
	return code
}

func containsOAuthSecret(text string, secrets []string) bool {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		variants := []string{
			secret,
			url.QueryEscape(secret),
			strings.ReplaceAll(url.QueryEscape(secret), "+", "%20"),
			url.PathEscape(secret),
		}
		for _, variant := range variants {
			if variant == "" {
				continue
			}
			textLower := strings.ToLower(text)
			variantLower := strings.ToLower(variant)
			// Very short test/development credentials (for example "v") must
			// not suppress unrelated standard codes such as invalid_grant. Exact
			// equality still protects them; real OAuth grants are long enough for
			// the substring check that catches an echoed value with a prefix.
			if text == variant || textLower == variantLower ||
				(len(variant) >= 8 && (strings.Contains(text, variant) || strings.Contains(textLower, variantLower))) {
				return true
			}
		}
	}
	return false
}

// checkOAuthSameOriginRedirect prevents a token endpoint from forwarding an
// authorization code or refresh token to a different origin. Go preserves the
// POST body for 307/308 redirects and can copy request headers on redirects, so
// the default redirect policy is not a safe credential boundary here.
func checkOAuthSameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errOAuthTooManyRedirects
	}
	if len(via) == 0 || sameOAuthOrigin(via[0].URL, req.URL) {
		return nil
	}
	return errOAuthCrossOriginRedirect
}

func sameOAuthOrigin(left, right *url.URL) bool {
	if left == nil || right == nil ||
		!strings.EqualFold(left.Scheme, right.Scheme) ||
		!strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return oauthEffectivePort(left) == oauthEffectivePort(right)
}

func oauthEffectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func doOAuthHTTPRequest(req *http.Request) (*http.Response, error) {
	resp, err := oauthHTTPClient.Do(req)
	if errors.Is(err, errOAuthCrossOriginRedirect) || errors.Is(err, errOAuthTooManyRedirects) {
		// http.Client wraps CheckRedirect errors in url.Error containing the
		// attacker-controlled Location. Return only the stable classification so
		// query/path credentials in that Location never reach logs or UI.
		if errors.Is(err, errOAuthCrossOriginRedirect) {
			return nil, errOAuthCrossOriginRedirect
		}
		return nil, errOAuthTooManyRedirects
	}
	return resp, err
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
