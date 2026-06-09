// Package mcpoauth adds OAuth 2.0 (PKCE) support for remote MCP servers.
// It discovers a server's authorization endpoints from its well-known
// metadata, dynamically registers a client when the server requires it
// (RFC 7591), runs the browser PKCE flow (reusing internal/auth's
// generic client), and persists + refreshes the resulting token so
// later sessions reconnect without a new browser round-trip.
//
// Wiring: an MCP server entry with auth="oauth" calls EnsureToken before
// connecting; the returned access token is attached as a Bearer header on
// the HTTP transport. Mirrors claude-code's remote-MCP OAuth.
package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

// asMetadata is the subset of RFC 8414 Authorization Server Metadata we
// use, plus the RFC 7591 registration endpoint.
type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// protectedResource is the subset of the MCP "protected resource"
// metadata used to locate the authorization server.
type protectedResource struct {
	AuthorizationServers []string `json:"authorization_servers"`
}

// httpClient is overridable in tests.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// Discover resolves the OAuth provider config for an MCP server URL. It
// tries the MCP protected-resource metadata first (to find the issuer),
// then the authorization-server metadata; failing that it probes the
// server origin's authorization-server metadata directly. When the AS
// advertises a registration endpoint, a public client is dynamically
// registered to obtain a client_id.
func Discover(ctx context.Context, serverURL, redirectURI string) (auth.OAuthProvider, error) {
	origin, err := originOf(serverURL)
	if err != nil {
		return auth.OAuthProvider{}, err
	}

	var md asMetadata
	// 1) protected-resource → issuer → AS metadata
	if pr, err := fetchProtectedResource(ctx, origin); err == nil && len(pr.AuthorizationServers) > 0 {
		if m, err := fetchASMetadata(ctx, pr.AuthorizationServers[0]); err == nil {
			md = m
		}
	}
	// 2) fall back to the server origin's AS metadata directly
	if md.TokenEndpoint == "" {
		if m, err := fetchASMetadata(ctx, origin); err == nil {
			md = m
		}
	}
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		return auth.OAuthProvider{}, fmt.Errorf("mcp oauth: could not discover authorization/token endpoints for %s", serverURL)
	}

	clientID := ""
	if md.RegistrationEndpoint != "" {
		id, err := registerClient(ctx, md.RegistrationEndpoint, redirectURI)
		if err == nil {
			clientID = id
		}
	}

	return auth.OAuthProvider{
		Name:            "mcp:" + origin,
		AuthURL:         md.AuthorizationEndpoint,
		TokenURL:        md.TokenEndpoint,
		ClientID:        clientID,
		Scopes:          md.ScopesSupported,
		UsePKCE:         true,
		HeaderTokenType: "Bearer",
	}, nil
}

func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("mcp oauth: bad server URL %q", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func fetchJSON(ctx context.Context, urlStr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", urlStr, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchProtectedResource(ctx context.Context, origin string) (protectedResource, error) {
	var pr protectedResource
	err := fetchJSON(ctx, origin+"/.well-known/oauth-protected-resource", &pr)
	return pr, err
}

func fetchASMetadata(ctx context.Context, issuer string) (asMetadata, error) {
	issuer = strings.TrimRight(issuer, "/")
	var md asMetadata
	err := fetchJSON(ctx, issuer+"/.well-known/oauth-authorization-server", &md)
	return md, err
}

// registerClient performs RFC 7591 dynamic client registration and
// returns the issued client_id. Public client (no secret), PKCE-capable.
func registerClient(ctx context.Context, regEndpoint, redirectURI string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "metis",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, regEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("client registration: HTTP %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("client registration: no client_id returned")
	}
	return out.ClientID, nil
}

// EnsureToken returns a valid access token for an MCP server, performing
// the least-disruptive path:
//
//  1. stored token still valid          → return it (no network)
//  2. stored token expired + refreshable → discover + refresh + persist
//  3. otherwise                          → discover + browser PKCE login
//
// serverKey identifies the stored entry (usually the server name);
// serverURL is the MCP endpoint used for discovery. The interactive
// branch (3) opens a browser; (1) and (2) are non-interactive.
func (s *TokenStore) EnsureToken(ctx context.Context, serverKey, serverURL string) (string, error) {
	if t, ok := s.Get(serverKey); ok && t.AccessToken != "" && !t.IsExpired() {
		return t.AccessToken, nil
	}
	// Representative loopback redirect for dynamic registration. Per
	// RFC 8252 §7.3 an AS must allow any loopback port, so registering
	// one port and completing on another (pickCallbackPort's range) is
	// spec-compliant.
	const redirect = "http://127.0.0.1:7700/callback"
	p, err := Discover(ctx, serverURL, redirect)
	if err != nil {
		return "", err
	}
	// Expired-but-refreshable.
	if t, ok := s.Get(serverKey); ok && t.RefreshToken != "" {
		if nt, rerr := auth.RefreshToken(p, t.RefreshToken); rerr == nil && nt.AccessToken != "" {
			_ = s.Put(serverKey, nt)
			return nt.AccessToken, nil
		}
	}
	// Fresh interactive login.
	nt, err := auth.OAuthLoginWithProvider(p, auth.OAuthOptions{})
	if err != nil {
		return "", err
	}
	if err := s.Put(serverKey, nt); err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

// TokenStore persists per-server OAuth tokens to ~/.metis/mcp-oauth.json.
type TokenStore struct {
	mu   sync.Mutex
	path string
}

// NewTokenStore opens (lazily) the per-user token store.
func NewTokenStore() *TokenStore {
	return &TokenStore{path: filepath.Join(config.Home(), "mcp-oauth.json")}
}

func (s *TokenStore) load() map[string]*auth.Token {
	m := map[string]*auth.Token{}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

// Get returns the stored token for a server key, if present.
func (s *TokenStore) Get(server string) (*auth.Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.load()[server]
	return t, ok
}

// Put stores (and persists) the token for a server key. The file is
// written 0600 — it holds bearer secrets.
func (s *TokenStore) Put(server string, t *auth.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	m[server] = t
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}
