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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
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
	Resource             string   `json:"resource"`
}

type discoveryResult struct {
	Provider    auth.OAuthProvider
	ServerURL   string
	ResourceURL string
	Issuer      string
}

const maxOAuthJSONResponseBytes = 1 << 20

type oauthHTTPStatusError struct {
	URL        string
	StatusCode int
}

func (e *oauthHTTPStatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d", e.URL, e.StatusCode)
}

// safeOAuthDiagnostic is implemented by auth's closed token-endpoint error.
// It deliberately exposes only the bounded fields that autonomous MCP launch
// paths may show to a model; remote response text is never part of this API.
type safeOAuthDiagnostic interface {
	OAuthStatusCode() int
	OAuthErrorCode() string
}

var (
	errOAuthCrossOriginRedirect = errors.New("mcp oauth: cross-origin redirect rejected")
	errOAuthTooManyRedirects    = errors.New("mcp oauth: stopped after 10 redirects")
	// httpClient is overridable in tests.
	httpClient = &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: checkSameOriginOAuthRedirect,
	}
)

// oauthLoginWithProvider is a narrow test seam around the browser flow. MCP
// owns a separate rich-token store, so every call through this seam must set
// OAuthOptions.SkipPersist and then persist the returned token via TokenStore.
var oauthLoginWithProvider = auth.OAuthLoginWithProviderContext

// Discover resolves the OAuth provider config for an MCP server URL. It
// tries the MCP protected-resource metadata first (to find the issuer),
// then the authorization-server metadata; failing that it probes the
// server origin's authorization-server metadata directly. When the AS
// advertises a registration endpoint, a public client is dynamically
// registered to obtain a client_id.
func Discover(ctx context.Context, serverURL string, redirectURIs []string) (auth.OAuthProvider, error) {
	result, err := discoverOAuth(ctx, serverURL, redirectURIs)
	if err != nil {
		return auth.OAuthProvider{}, err
	}
	return result.Provider, nil
}

func discoverOAuth(ctx context.Context, serverURL string, redirectURIs []string) (discoveryResult, error) {
	canonicalServerURL, err := canonicalOAuthURL(serverURL)
	if err != nil {
		return discoveryResult{}, err
	}
	origin, err := originOf(canonicalServerURL)
	if err != nil {
		return discoveryResult{}, err
	}

	var md asMetadata
	resourceURL := canonicalServerURL
	issuerURL, err := canonicalIssuerURL(origin)
	if err != nil {
		return discoveryResult{}, err
	}
	// 1) protected-resource → issuer → AS metadata
	pr, protectedErr := fetchProtectedResource(ctx, canonicalServerURL)
	if protectedErr == nil {
		if strings.TrimSpace(pr.Resource) == "" {
			return discoveryResult{}, fmt.Errorf("mcp oauth: protected resource metadata omitted required resource identifier")
		}
		canonical, canonicalErr := canonicalOAuthURL(pr.Resource)
		if canonicalErr != nil {
			return discoveryResult{}, fmt.Errorf("mcp oauth: invalid protected resource URL: %w", canonicalErr)
		}
		if canonical != canonicalServerURL {
			return discoveryResult{}, fmt.Errorf(
				"mcp oauth: protected resource identifier mismatch: requested %q, metadata returned %q",
				canonicalServerURL, canonical,
			)
		}
		resourceURL = canonical
	} else if !isOAuthHTTPStatus(protectedErr, http.StatusNotFound) {
		return discoveryResult{}, fmt.Errorf("mcp oauth: protected resource metadata for %s: %w", canonicalServerURL, protectedErr)
	}
	if protectedErr == nil && len(pr.AuthorizationServers) > 0 {
		issuerURL, err = canonicalIssuerURL(pr.AuthorizationServers[0])
		if err != nil {
			return discoveryResult{}, fmt.Errorf("mcp oauth: invalid explicit authorization server: %w", err)
		}
		m, metadataErr := fetchASMetadata(ctx, issuerURL)
		if metadataErr != nil {
			return discoveryResult{}, fmt.Errorf(
				"mcp oauth: explicit authorization server metadata for %s: %w",
				issuerURL, metadataErr,
			)
		}
		md = m
	} else {
		// No explicit issuer was advertised, so the server origin is the only
		// permissible fallback. Once an explicit issuer exists, failure above is
		// terminal and must never be replaced with origin metadata.
		m, metadataErr := fetchASMetadata(ctx, issuerURL)
		if metadataErr != nil {
			return discoveryResult{}, fmt.Errorf("mcp oauth: authorization server metadata for %s: %w", issuerURL, metadataErr)
		}
		md = m
	}
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		return discoveryResult{}, fmt.Errorf("mcp oauth: could not discover authorization/token endpoints for %s", serverURL)
	}
	if strings.TrimSpace(md.Issuer) == "" {
		return discoveryResult{}, fmt.Errorf("mcp oauth: authorization server metadata omitted issuer")
	}
	metadataIssuer, err := canonicalIssuerURL(md.Issuer)
	if err != nil {
		return discoveryResult{}, fmt.Errorf("mcp oauth: invalid discovered issuer: %w", err)
	}
	if metadataIssuer != issuerURL {
		return discoveryResult{}, fmt.Errorf(
			"mcp oauth: authorization server issuer mismatch: selected %q, metadata returned %q",
			issuerURL, metadataIssuer,
		)
	}
	md.AuthorizationEndpoint, err = canonicalOAuthURL(md.AuthorizationEndpoint)
	if err != nil {
		return discoveryResult{}, fmt.Errorf("mcp oauth: invalid authorization endpoint: %w", err)
	}
	md.TokenEndpoint, err = canonicalOAuthURL(md.TokenEndpoint)
	if err != nil {
		return discoveryResult{}, fmt.Errorf("mcp oauth: invalid token endpoint: %w", err)
	}
	if strings.TrimSpace(md.RegistrationEndpoint) != "" {
		md.RegistrationEndpoint, err = canonicalOAuthURL(md.RegistrationEndpoint)
		if err != nil {
			return discoveryResult{}, fmt.Errorf("mcp oauth: invalid registration endpoint: %w", err)
		}
	}

	clientID := ""
	if md.RegistrationEndpoint != "" {
		id, err := registerClient(ctx, md.RegistrationEndpoint, redirectURIs)
		if err != nil {
			return discoveryResult{}, fmt.Errorf("mcp oauth: dynamic client registration: %w", err)
		}
		clientID = id
	}

	provider := auth.OAuthProvider{
		Name:            "mcp:" + origin,
		AuthURL:         md.AuthorizationEndpoint,
		TokenURL:        md.TokenEndpoint,
		ClientID:        clientID,
		Scopes:          md.ScopesSupported,
		UsePKCE:         true,
		ResourceURL:     resourceURL,
		HeaderTokenType: "Bearer",
	}
	return discoveryResult{
		Provider: provider, ServerURL: canonicalServerURL,
		ResourceURL: resourceURL, Issuer: issuerURL,
	}, nil
}

func originOf(raw string) (string, error) {
	canonical, err := canonicalOAuthURL(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(canonical)
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
	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &oauthHTTPStatusError{URL: urlStr, StatusCode: resp.StatusCode}
	}
	if resp.ContentLength > maxOAuthJSONResponseBytes {
		return fmt.Errorf("OAuth JSON response is too large (limit %d bytes)", maxOAuthJSONResponseBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthJSONResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxOAuthJSONResponseBytes {
		return fmt.Errorf("OAuth JSON response is too large (limit %d bytes)", maxOAuthJSONResponseBytes)
	}
	return json.Unmarshal(raw, out)
}

func isOAuthHTTPStatus(err error, status int) bool {
	var statusErr *oauthHTTPStatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == status
}

func fetchProtectedResource(ctx context.Context, resourceIdentifier string) (protectedResource, error) {
	var pr protectedResource
	metadataURL, err := oauthWellKnownURL(resourceIdentifier, "oauth-protected-resource", true)
	if err != nil {
		return pr, err
	}
	err = fetchJSON(ctx, metadataURL, &pr)
	return pr, err
}

func fetchASMetadata(ctx context.Context, issuer string) (asMetadata, error) {
	var md asMetadata
	metadataURL, err := oauthWellKnownURL(issuer, "oauth-authorization-server", false)
	if err != nil {
		return md, err
	}
	err = fetchJSON(ctx, metadataURL, &md)
	return md, err
}

func canonicalIssuerURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("mcp oauth: bad issuer URL %q", raw)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fmt.Errorf("mcp oauth: issuer URL must not contain a query or fragment")
	}
	return canonicalOAuthURL(raw)
}

// oauthWellKnownURL implements the insertion rule from RFC 8414 section 3
// and RFC 9728 section 3: the well-known suffix is inserted between the host
// and the identifier path, rather than appended after a tenant path.
func oauthWellKnownURL(identifier, suffix string, preserveQuery bool) (string, error) {
	canonical, err := canonicalOAuthURL(identifier)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return "", err
	}
	escapedPath := strings.TrimRight(u.EscapedPath(), "/")
	if escapedPath == "/" {
		escapedPath = ""
	}
	rawPath := "/.well-known/" + suffix + escapedPath
	decodedPath, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", fmt.Errorf("mcp oauth: invalid identifier path: %w", err)
	}
	u.Path = decodedPath
	u.RawPath = rawPath
	if !preserveQuery {
		u.RawQuery = ""
		u.ForceQuery = false
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

// registerClient performs RFC 7591 dynamic client registration and
// returns the issued client_id. Public client (no secret), PKCE-capable.
func registerClient(ctx context.Context, regEndpoint string, redirectURIs []string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "metis",
		"redirect_uris":              redirectURIs,
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
	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("client registration: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxOAuthJSONResponseBytes {
		return "", fmt.Errorf("client registration response is too large (limit %d bytes)", maxOAuthJSONResponseBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthJSONResponseBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxOAuthJSONResponseBytes {
		return "", fmt.Errorf("client registration response is too large (limit %d bytes)", maxOAuthJSONResponseBytes)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("client registration: no client_id returned")
	}
	return out.ClientID, nil
}

func checkSameOriginOAuthRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errOAuthTooManyRedirects
	}
	if len(via) == 0 || sameOAuthHTTPOrigin(via[0].URL, req.URL) {
		return nil
	}
	return errOAuthCrossOriginRedirect
}

func sameOAuthHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil ||
		!strings.EqualFold(left.Scheme, right.Scheme) ||
		!strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return oauthHTTPPort(left) == oauthHTTPPort(right)
}

func oauthHTTPPort(u *url.URL) string {
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
	resp, err := httpClient.Do(req)
	if errors.Is(err, errOAuthCrossOriginRedirect) || errors.Is(err, errOAuthTooManyRedirects) {
		// http.Client includes the attacker-controlled Location URL in its
		// redirect error. Keep that value out of CLI/Desktop diagnostics.
		if errors.Is(err, errOAuthCrossOriginRedirect) {
			return nil, errOAuthCrossOriginRedirect
		}
		return nil, errOAuthTooManyRedirects
	}
	return resp, err
}

// EnsureToken returns a valid access token for an MCP server, performing
// the least-disruptive path:
//
//  1. stored token still valid          → return it (no network)
//  2. stored token expired + refreshable → refresh with its persisted
//     issuer/client registration and persist
//  3. otherwise → fail without network in non-interactive mode, or run an
//     explicit discovery + browser PKCE login in interactive mode
//
// serverKey identifies the stored entry (usually the server name);
// serverURL is the MCP endpoint used for discovery. The interactive
// branch (3) opens a browser and is taken ONLY when interactive=true —
// autonomous connect paths (tool calls, headless runs) pass false so a
// missing token surfaces as an error instead of hanging on a browser
// flow that may never complete.
func (s *TokenStore) EnsureToken(ctx context.Context, serverKey, serverURL string, interactive bool) (string, error) {
	canonicalServerURL, err := canonicalOAuthURL(serverURL)
	if err != nil {
		return "", err
	}
	entry, err := s.GetEntry(serverKey)
	if err != nil {
		return "", err
	}
	if entry != nil && entry.boundTo(canonicalServerURL) {
		if entry.Token.AccessToken != "" && !entry.Token.IsExpired() {
			return entry.Token.AccessToken, nil
		}
		if strings.TrimSpace(entry.Token.RefreshToken) != "" {
			accessToken, reauthErr, fatalErr := s.refreshStoredToken(ctx, serverKey, canonicalServerURL)
			if fatalErr != nil {
				return "", fatalErr
			}
			if accessToken != "" {
				return accessToken, nil
			}
			if !interactive {
				return "", fmt.Errorf(
					"%w for server %q: %s",
					ErrCredentialReauthRequired, serverKey, autonomousRefreshFailure(reauthErr),
				)
			}
		} else if !interactive {
			return "", fmt.Errorf("%w for server %q: access token expired and no refresh token is stored", ErrCredentialReauthRequired, serverKey)
		}
	} else if entry != nil && !interactive {
		reason := "stored credential has no canonical server/resource binding"
		if entry.ServerURL != "" && entry.ServerURL != canonicalServerURL {
			reason = fmt.Sprintf("stored credential is bound to %q, not %q", entry.ServerURL, canonicalServerURL)
		}
		return "", fmt.Errorf("%w for server %q: %s", ErrCredentialReauthRequired, serverKey, reason)
	} else if entry == nil && !interactive {
		return "", fmt.Errorf("%w for server %q; run `/mcp login %s`", ErrCredentialMissing, serverKey, serverKey)
	}

	// Only the explicit interactive login path may perform discovery or dynamic
	// client registration when no usable refresh credential exists.
	return s.loginCanonical(ctx, serverKey, canonicalServerURL)
}

func autonomousRefreshFailure(err error) string {
	message := "OAuth refresh failed"
	var diagnostic safeOAuthDiagnostic
	if !errors.As(err, &diagnostic) {
		return message
	}
	status := diagnostic.OAuthStatusCode()
	if status < 100 || status > 599 {
		status = 0
	}
	code := boundedOAuthDiagnosticCode(diagnostic.OAuthErrorCode())
	detail := ""
	if status > 0 {
		detail = fmt.Sprintf("HTTP %d", status)
	}
	if code != "" {
		if detail != "" {
			detail += ", "
		}
		detail += "code " + code
	}
	if detail == "" {
		return message
	}
	return message + " (" + detail + ")"
}

func boundedOAuthDiagnosticCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 64 {
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

// Login performs a fresh interactive OAuth authorization even when the token
// store already contains an unexpired credential. This is the contract behind
// an explicit `/mcp login`: a token can be revoked server-side before its local
// expiry, and re-running login must therefore not silently reuse it.
//
// Autonomous startup and tool execution must use EnsureToken with
// interactive=false; they never call this method and cannot open a browser.
func (s *TokenStore) Login(ctx context.Context, serverKey, serverURL string) (string, error) {
	canonicalServerURL, err := canonicalOAuthURL(serverURL)
	if err != nil {
		return "", err
	}
	return s.loginCanonical(ctx, serverKey, canonicalServerURL)
}

func (s *TokenStore) loginCanonical(ctx context.Context, serverKey, canonicalServerURL string) (string, error) {
	redirects := make([]string, 0, 20)
	for port := 7700; port < 7720; port++ {
		redirects = append(redirects, fmt.Sprintf("http://127.0.0.1:%d/callback", port))
	}
	discovered, err := discoverOAuth(ctx, canonicalServerURL, redirects)
	if err != nil {
		return "", err
	}
	newToken, err := oauthLoginWithProvider(ctx, discovered.Provider, auth.OAuthOptions{SkipPersist: true})
	if err != nil {
		return "", err
	}
	if newToken == nil || strings.TrimSpace(newToken.AccessToken) == "" {
		return "", fmt.Errorf("mcp oauth: login for %q returned no access token", serverKey)
	}
	entry := &TokenEntry{
		ServerURL: discovered.ServerURL, ResourceURL: discovered.ResourceURL,
		Issuer: discovered.Issuer, ClientID: discovered.Provider.ClientID,
		AuthURL: discovered.Provider.AuthURL, TokenURL: discovered.Provider.TokenURL,
		Scopes: append([]string(nil), discovered.Provider.Scopes...), Token: newToken,
	}
	if err := s.PutEntry(serverKey, entry); err != nil {
		return "", fmt.Errorf("mcp oauth: persist login credential for %q: %w", serverKey, err)
	}
	return newToken.AccessToken, nil
}

// refreshStoredToken serializes the complete load/refresh/save transaction by
// server across goroutines and CLI/Desktop processes. It re-reads the entry
// after acquiring the lease so a waiter observes a token another process has
// already rotated instead of replaying a single-use refresh token.
func (s *TokenStore) refreshStoredToken(ctx context.Context, serverKey, canonicalServerURL string) (accessToken string, reauthErr, fatalErr error) {
	err := withTokenRefreshLease(ctx, s.path, serverKey, func() error {
		entry, err := s.GetEntry(serverKey)
		if err != nil {
			fatalErr = err
			return nil
		}
		if entry == nil {
			reauthErr = errors.New("stored credential disappeared before refresh")
			return nil
		}
		if !entry.boundTo(canonicalServerURL) {
			reauthErr = errors.New("stored credential binding changed before refresh")
			return nil
		}
		if entry.Token.AccessToken != "" && !entry.Token.IsExpired() {
			accessToken = entry.Token.AccessToken
			return nil
		}
		if strings.TrimSpace(entry.Token.RefreshToken) == "" {
			reauthErr = errors.New("access token expired and no refresh token is stored")
			return nil
		}
		provider, err := entry.provider()
		if err != nil {
			reauthErr = err
			return nil
		}
		refreshed, err := auth.RefreshTokenContext(ctx, provider, entry.Token.RefreshToken)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				fatalErr = err
			} else {
				reauthErr = err
			}
			return nil
		}
		if refreshed == nil || strings.TrimSpace(refreshed.AccessToken) == "" {
			reauthErr = errors.New("refresh endpoint returned no access token")
			return nil
		}
		updated := cloneTokenEntry(entry)
		updated.Token = refreshed
		if err := s.PutEntry(serverKey, updated); err != nil {
			fatalErr = fmt.Errorf("mcp oauth: persist refreshed credential for %q: %w", serverKey, err)
			return nil
		}
		accessToken = refreshed.AccessToken
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("mcp oauth: refresh lease for %q: %w", serverKey, err)
	}
	if fatalErr != nil {
		return "", nil, fatalErr
	}
	if accessToken == "" && reauthErr == nil {
		reauthErr = errors.New("stored credential could not be refreshed")
	}
	return accessToken, reauthErr, nil
}
