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
//  6. Persist the token in the OAuth store, unless the caller owns a separate
//     credential store and opts out with OAuthOptions.SkipPersist
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
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// oauthHTTPClient bounds the code→token exchange and refresh calls. Browser
// authorization is controlled by the caller context so MFA/SSO users are not
// cut off by an arbitrary local deadline. Token exchange used
// http.DefaultClient (NO timeout): a wedged or malicious
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

// OAuthProvider describes one OAuth 2.0 endpoint set. Canonical LLM logins
// select one of KnownProviders through `metis login`.
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

	// CallbackAddress pins the local listener for providers that register one
	// exact loopback redirect. CallbackRedirectURL is the URI sent to the
	// provider (some registrations use localhost while the listener binds only
	// 127.0.0.1). CallbackPath limits which local route may complete the flow.
	CallbackAddress     string
	CallbackRedirectURL string
	CallbackPath        string

	// TokenRequestJSON selects JSON token exchange/refresh requests instead of
	// application/x-www-form-urlencoded. IncludeStateInTokenRequest is required
	// by Anthropic's subscriber flow.
	TokenRequestJSON           bool
	IncludeStateInTokenRequest bool
	OmitScopesOnRefresh        bool

	// ManualRedirectURL — if set, enables "manual paste" mode for
	// non-browser environments (SSH, headless, locked-down corp). When
	// the user passes --manual the auth URL is built with this as the
	// redirect_uri instead of localhost; the provider then displays the
	// auth code on a static page and the user pastes it back to the
	// terminal. This is the same browser/manual handoff shape used by Pi.
	ManualRedirectURL string
}

// KnownProviders ships ready-to-use configs for supported OAuth providers.
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
	// Anthropic Claude Pro/Max subscriber OAuth. The client id, redirect and
	// scopes are public protocol constants used by Pi's Claude-compatible flow;
	// there is deliberately no client secret in this native-app PKCE client.
	"anthropic": {
		Name:                       "anthropic",
		AuthURL:                    "https://claude.ai/oauth/authorize",
		TokenURL:                   "https://platform.claude.com/v1/oauth/token",
		ClientID:                   "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:                     []string{"org:create_api_key", "user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload"},
		UsePKCE:                    true,
		HeaderTokenType:            "Bearer",
		CallbackAddress:            "127.0.0.1:53692",
		CallbackRedirectURL:        "http://localhost:53692/callback",
		CallbackPath:               "/callback",
		ManualRedirectURL:          "http://localhost:53692/callback",
		TokenRequestJSON:           true,
		IncludeStateInTokenRequest: true,
		OmitScopesOnRefresh:        true,
		ExtraParams:                map[string]string{"code": "true"},
	},
	// Deprecated compatibility spelling used by older METIS releases. Its Name
	// remains "anthropic" so even the legacy auth.json path does not create a
	// second provider identity. Rich-store APIs canonicalize this alias too.
	"anthropic-claudeai": {
		Name:                       "anthropic",
		AuthURL:                    "https://claude.ai/oauth/authorize",
		TokenURL:                   "https://platform.claude.com/v1/oauth/token",
		ClientID:                   "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:                     []string{"org:create_api_key", "user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload"},
		UsePKCE:                    true,
		HeaderTokenType:            "Bearer",
		CallbackAddress:            "127.0.0.1:53692",
		CallbackRedirectURL:        "http://localhost:53692/callback",
		CallbackPath:               "/callback",
		ManualRedirectURL:          "http://localhost:53692/callback",
		TokenRequestJSON:           true,
		IncludeStateInTokenRequest: true,
		OmitScopesOnRefresh:        true,
		ExtraParams:                map[string]string{"code": "true"},
	},
	// OpenAI Codex subscription OAuth (ChatGPT Plus/Pro). This is a distinct
	// provider from platform OpenAI API-key auth. The public native-app client
	// id and fixed callback are protocol constants; no client secret is used.
	"openai-codex": {
		Name:                "openai-codex",
		AuthURL:             "https://auth.openai.com/oauth/authorize",
		TokenURL:            "https://auth.openai.com/oauth/token",
		ClientID:            "app_EMoamEEZ73f0CkXaXp7hrann",
		Scopes:              []string{"openid", "profile", "email", "offline_access"},
		UsePKCE:             true,
		HeaderTokenType:     "Bearer",
		CallbackAddress:     "127.0.0.1:1455",
		CallbackRedirectURL: "http://localhost:1455/auth/callback",
		CallbackPath:        "/auth/callback",
		ManualRedirectURL:   "http://localhost:1455/auth/callback",
		OmitScopesOnRefresh: true,
		ExtraParams: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "metis",
		},
	},
	// gitlab / slack / discord can be added by users via the
	// override file — kept out of defaults to avoid a giant table
	// that few users will touch.
}

// OAuthLogin is the legacy low-level browser login. Canonical LLM login uses
// LoginOAuthCredential so refresh metadata is kept in llm-oauth.json.
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
	// SkipPersist returns the rich token without copying it into llm-oauth.json.
	// This is intended for callers with their own credential store
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
	// browser. Mirrors Pi's manual browser handoff while keeping provider-
	// specific protocol details in this package.
	Manual bool

	// PasteCode supplies the auth code in Manual mode. Called after
	// the auth URL has been displayed to the user. Returning a non-nil
	// error aborts the flow. If nil under Manual=true, OAuthLoginOpts
	// reads a single line from os.Stdin (interactive default).
	PasteCode func(authURL string) (string, error)

	// PasteCodeContext is the cancellable form preferred by UI clients. In
	// automatic mode it also enables a same-run pasted-redirect fallback, raced
	// against the localhost callback. This keeps login usable when the fixed
	// callback port is occupied or the browser cannot reach localhost. The
	// context is canceled when the login screen/session is closed. PasteCode is
	// retained for source compatibility; the outer login call can stop waiting
	// for it, but a legacy callback that ignores its own lifecycle may continue
	// running in the background.
	PasteCodeContext func(context.Context, string) (string, error)

	// FallbackPasteCodeContext is used only when an automatic flow cannot bind
	// its registered localhost callback address. Unlike PasteCodeContext it is
	// never raced against a working callback, so blocking terminal readers may
	// safely use it.
	FallbackPasteCodeContext func(context.Context, string) (string, error)

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
// the knobs. By default, success persists the credential to the OAuth store
// and returns the rich Token. OAuth tokens are never written as LLM API keys.
// Callers with a dedicated credential store can set SkipPersist and persist
// the returned access + refresh + expiry fields themselves.
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
	provider = canonicalOAuthProviderID(strings.TrimSpace(provider))
	p, ok := KnownProviders[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %s)",
			provider, strings.Join(knownNames(), ", "))
	}
	if opts.Manual && p.ManualRedirectURL == "" {
		return nil, fmt.Errorf("provider %q has no ManualRedirectURL — manual mode unsupported", provider)
	}

	verifier, challenge, err := makePKCEChecked()
	if err != nil {
		return nil, err
	}
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
	listener, redirectURI, callbackPath, err := pickCallbackListener(p)
	if err != nil {
		fallback := opts.FallbackPasteCodeContext
		if fallback == nil {
			fallback = opts.PasteCodeContext
		}
		if p.ManualRedirectURL != "" && (fallback != nil || opts.PasteCode != nil) {
			manualOpts := opts
			manualOpts.Manual = true
			manualOpts.PasteCodeContext = fallback
			return runOAuthManualContext(ctx, p, verifier, challenge, state, manualOpts)
		}
		return nil, err
	}
	defer listener.Close()

	authURL := buildAuthURL(p, redirectURI, state, challenge, false)
	if opts.AuthURLHandler != nil {
		if err := opts.AuthURLHandler(authURL); err != nil {
			return nil, fmt.Errorf("oauth: AuthURLHandler: %w", err)
		}
	} else {
		fmt.Printf("Open this URL to authorize:\n  %s\n", authURL)
		if err := openBrowser(authURL); err != nil {
			fmt.Printf("Browser could not be opened automatically; use the URL above.\n")
		}
	}

	resultCh := make(chan oauthCallbackResult, 1)
	srv := newCallbackServerAtPath(callbackPath, state, resultCh)
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

	type pastedCodeResult struct {
		code  string
		state string
		err   error
	}
	var pasteCh <-chan pastedCodeResult
	var cancelPaste context.CancelFunc
	var pasteDone <-chan struct{}
	if opts.PasteCodeContext != nil {
		pasteCtx, cancel := context.WithCancel(ctx)
		cancelPaste = cancel
		ch := make(chan pastedCodeResult, 1)
		done := make(chan struct{})
		pasteCh = ch
		pasteDone = done
		go func() {
			defer close(done)
			input, waitErr := opts.PasteCodeContext(pasteCtx, authURL)
			if waitErr != nil {
				ch <- pastedCodeResult{err: waitErr}
				return
			}
			code, pastedState, parseErr := parseManualAuthorizationInput(input)
			ch <- pastedCodeResult{code: code, state: pastedState, err: parseErr}
		}()
	}
	stopPaste := func() {
		if cancelPaste != nil {
			cancelPaste()
			<-pasteDone
			cancelPaste = nil
		}
	}
	defer stopPaste()
	var code string
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}
		code = result.code
		stopPaste()
	case pasted := <-pasteCh:
		if pasted.err != nil {
			return nil, pasted.err
		}
		if pasted.state != "" && pasted.state != state {
			return nil, errors.New("oauth: state mismatch in pasted authorization response")
		}
		code = pasted.code
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := exchangeCodeForTokenFullWithStateContext(ctx, p, code, state, redirectURI, verifier)
	if err != nil {
		return nil, err
	}
	if !opts.SkipPersist {
		if err := persistOAuthToken(p.Name, tok); err != nil {
			return nil, fmt.Errorf("save OAuth credential: %w", err)
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
	code, pastedState, err := parseManualAuthorizationInput(code)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pastedState != "" && pastedState != state {
		return nil, errors.New("oauth: state mismatch in pasted authorization response")
	}

	tok, err := exchangeCodeForTokenFullWithStateContext(ctx, p, code, state, p.ManualRedirectURL, verifier)
	if err != nil {
		return nil, err
	}
	if !opts.SkipPersist {
		if err := persistOAuthToken(p.Name, tok); err != nil {
			return nil, fmt.Errorf("save OAuth credential: %w", err)
		}
	}
	return tok, nil
}

func persistOAuthToken(provider string, token *Token) error {
	credential, err := credentialFromToken(canonicalOAuthProviderID(provider), token)
	if err != nil {
		return err
	}
	return PutOAuth(provider, *credential)
}

func knownNames() []string {
	names := make([]string, 0, len(KnownProviders))
	for k := range KnownProviders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

var oauthRandomReader io.Reader = rand.Reader

func makePKCEChecked() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := io.ReadFull(oauthRandomReader, b); err != nil {
		return "", "", errors.New("oauth: secure random generation failed")
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	_, challenge = makePKCETestSeam(verifier)
	return verifier, challenge, nil
}

// makePKCE is retained for package-level compatibility. Production login
// paths call makePKCEChecked so entropy failures are never ignored.
func makePKCE() (verifier, challenge string) {
	verifier, challenge, _ = makePKCEChecked()
	return verifier, challenge
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
	if _, err := io.ReadFull(oauthRandomReader, b); err != nil {
		return "", errors.New("oauth: secure random generation failed")
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

func pickCallbackListener(p OAuthProvider) (net.Listener, string, string, error) {
	callbackPath := strings.TrimSpace(p.CallbackPath)
	if callbackPath == "" {
		callbackPath = "/callback"
	}
	if !strings.HasPrefix(callbackPath, "/") || strings.ContainsAny(callbackPath, "?#") {
		return nil, "", "", errors.New("oauth: invalid callback path")
	}
	if p.CallbackAddress != "" {
		listener, err := net.Listen("tcp", p.CallbackAddress)
		if err != nil {
			return nil, "", "", fmt.Errorf("oauth: fixed callback listener unavailable; retry with manual login: %w", err)
		}
		redirectURI := strings.TrimSpace(p.CallbackRedirectURL)
		if redirectURI == "" {
			redirectURI = "http://" + listener.Addr().String() + callbackPath
		}
		if err := validateLoopbackRedirectURI(redirectURI, callbackPath); err != nil {
			_ = listener.Close()
			return nil, "", "", err
		}
		return listener, redirectURI, callbackPath, nil
	}
	listener, err := pickCallbackPort()
	if err != nil {
		return nil, "", "", err
	}
	return listener, "http://" + listener.Addr().String() + callbackPath, callbackPath, nil
}

func validateLoopbackRedirectURI(raw, expectedPath string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("oauth: invalid loopback callback redirect")
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("oauth: callback redirect must use a loopback host")
		}
	}
	if u.Path != expectedPath {
		return errors.New("oauth: callback redirect path mismatch")
	}
	return nil
}

func buildAuthURL(p OAuthProvider, redirectURI, state, challenge string, _ bool) string {
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
	if term.IsTerminal(int(os.Stdin.Fd())) {
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err == nil {
			return strings.TrimSpace(string(value))
		}
	}
	var s string
	_, _ = fmt.Scanln(&s)
	return s
}

type oauthCallbackResult struct {
	code string
	err  error
}

func newCallbackServer(state string, resultCh chan<- oauthCallbackResult) *http.Server {
	return newCallbackServerAtPath("/callback", state, resultCh)
}

func newCallbackServerAtPath(callbackPath, state string, resultCh chan<- oauthCallbackResult) *http.Server {
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
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch — possible CSRF, refusing", http.StatusBadRequest)
			// Invalid requests must not consume the one legitimate callback. An
			// unauthenticated local request could otherwise cancel login (DoS).
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
		if code == "" || len(code) > 16<<10 {
			http.Error(w, "missing code", http.StatusBadRequest)
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

func parseManualAuthorizationInput(input string) (code, state string, err error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", "", errors.New("oauth: no authorization code provided")
	}
	if len(value) > 32<<10 {
		return "", "", errors.New("oauth: pasted authorization response is too large")
	}
	if parsed, parseErr := url.Parse(value); parseErr == nil && parsed.Scheme != "" && parsed.Host != "" {
		code = parsed.Query().Get("code")
		state = parsed.Query().Get("state")
	} else if strings.Contains(value, "code=") {
		params, parseErr := url.ParseQuery(strings.TrimPrefix(value, "?"))
		if parseErr != nil {
			return "", "", errors.New("oauth: invalid pasted authorization response")
		}
		code, state = params.Get("code"), params.Get("state")
	} else if strings.Contains(value, "#") {
		parts := strings.SplitN(value, "#", 2)
		code, state = parts[0], parts[1]
	} else {
		code = value
	}
	code, state = strings.TrimSpace(code), strings.TrimSpace(state)
	if code == "" {
		return "", "", errors.New("oauth: no authorization code provided")
	}
	if len(code) > 16<<10 || len(state) > 1024 || strings.ContainsAny(code, "\r\n") || strings.ContainsAny(state, "\r\n") {
		return "", "", errors.New("oauth: invalid pasted authorization response")
	}
	return code, state, nil
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

func waitForManualCode(ctx context.Context, authURL string, opts OAuthOptions) (string, error) {
	// Context-aware readers own their cancellation contract and are invoked in
	// the foreground. In particular, this prevents a Windows ReadPassword call
	// from being abandoned in a goroutine with console echo still disabled when
	// the outer context is cancelled. Unix readers poll ctx and return promptly;
	// Windows restores the console before returning from ReadPassword.
	if opts.PasteCodeContext != nil {
		return opts.PasteCodeContext(ctx, authURL)
	}

	// Legacy callbacks do not accept a context. Keep the historical early-
	// cancellation behavior for embedders, but never use this branch for the
	// built-in terminal reader.
	type manualCodeResult struct {
		code string
		err  error
	}
	resultCh := make(chan manualCodeResult, 1)
	go func() {
		var code string
		var err error
		switch {
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
	return exchangeCodeForTokenFullWithStateContext(ctx, p, code, "", redirectURI, verifier)
}

func exchangeCodeForTokenFullWithStateContext(ctx context.Context, p OAuthProvider, code, state, redirectURI, verifier string) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const operation = "token exchange"
	params := map[string]string{
		"client_id": p.ClientID, "code": code, "redirect_uri": redirectURI,
		"grant_type": "authorization_code",
	}
	if p.ResourceURL != "" {
		params["resource"] = p.ResourceURL
	}
	if p.UsePKCE {
		params["code_verifier"] = verifier
	}
	if p.IncludeStateInTokenRequest {
		if state == "" {
			return nil, newOAuthEndpointError(operation, 0, "missing_state", code, verifier)
		}
		params["state"] = state
	}

	req, err := newOAuthTokenRequest(ctx, p, params)
	if err != nil {
		return nil, newOAuthEndpointError(operation, 0, "invalid_request", code, verifier, state)
	}

	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return nil, safeOAuthRequestError(ctx, operation, err, code, verifier, state)
	}
	defer resp.Body.Close()
	return decodeOAuthTokenResponse(resp, operation, []string{code, verifier, state})
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
		if v > 0 && v <= float64((100*365*24*time.Hour)/time.Second) {
			tok.ExpiresAt = time.Now().Add(time.Duration(v) * time.Second)
		}
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= int64(100*365*24*60*60) {
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
	verifier, challenge, err := makePKCEChecked()
	if err != nil {
		return nil, err
	}
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
	params := map[string]string{"grant_type": "refresh_token", "refresh_token": refreshToken}
	if p.ClientID != "" {
		params["client_id"] = p.ClientID
	}
	if len(p.Scopes) > 0 && !p.OmitScopesOnRefresh {
		params["scope"] = strings.Join(p.Scopes, " ")
	}
	if p.ResourceURL != "" {
		params["resource"] = p.ResourceURL
	}
	req, err := newOAuthTokenRequest(ctx, p, params)
	if err != nil {
		return nil, newOAuthEndpointError(operation, 0, "invalid_request", refreshToken)
	}
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

func newOAuthTokenRequest(ctx context.Context, provider OAuthProvider, params map[string]string) (*http.Request, error) {
	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	if provider.TokenRequestJSON {
		payload, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(payload))
		contentType = "application/json"
	} else {
		form := url.Values{}
		for key, value := range params {
			if value != "" {
				form.Set(key, value)
			}
		}
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	return req, nil
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
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
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
