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
func OAuthLogin(provider string) (string, error) {
	p, ok := KnownProviders[provider]
	if !ok {
		return "", fmt.Errorf("unknown provider %q (known: %s)",
			provider, strings.Join(knownNames(), ", "))
	}
	verifier, challenge := makePKCE()
	state, err := randomState()
	if err != nil {
		return "", err
	}

	// 1. Find a free localhost port for the callback.
	listener, err := pickCallbackPort()
	if err != nil {
		return "", err
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/callback"

	// 2. Build auth URL and open browser.
	authURL := buildAuthURL(p, redirectURI, state, challenge)
	if err := openBrowser(authURL); err != nil {
		// Browser open failed — print URL for manual paste.
		fmt.Printf("Open this URL to authorize:\n  %s\n", authURL)
	}

	// 3. Wait for callback.
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := newCallbackServer(state, codeCh, errCh)
	go func() { _ = srv.Serve(listener) }()
	defer srv.Shutdown(context.Background())

	var code string
	select {
	case code = <-codeCh:
		// got it
	case e := <-errCh:
		return "", e
	case <-time.After(2 * time.Minute):
		return "", fmt.Errorf("oauth: user did not authorize within 2 minutes")
	}

	// 4. Exchange code for token.
	token, err := exchangeCodeForToken(p, code, redirectURI, verifier)
	if err != nil {
		return "", err
	}

	// 5. Persist via existing auth.Set so the rest of metis sees the
	// token under the provider's name.
	if err := Set(provider, token); err != nil {
		return "", fmt.Errorf("token saved-to-auth: %w", err)
	}
	return token, nil
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
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
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

func buildAuthURL(p OAuthProvider, redirectURI, state, challenge string) string {
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
	for k, v := range p.ExtraParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(p.AuthURL, "?") {
		sep = "&"
	}
	return p.AuthURL + sep + q.Encode()
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

func exchangeCodeForToken(p OAuthProvider, code, redirectURI, verifier string) (string, error) {
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
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token endpoint %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		// Some providers (GitHub) return form-encoded — try that.
		// Already consumed body; bail.
		return "", fmt.Errorf("token endpoint: bad response: %w", err)
	}
	if t, ok := raw["access_token"].(string); ok && t != "" {
		return t, nil
	}
	if e, ok := raw["error"].(string); ok {
		return "", fmt.Errorf("token endpoint: %s", e)
	}
	return "", fmt.Errorf("token endpoint: no access_token in response")
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
