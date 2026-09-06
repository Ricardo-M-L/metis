package mcpoauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

const (
	tokenEntryFormatVersion = 2
	tokenStoreMaxJSONBytes  = 4 << 20
)

var (
	// ErrCredentialMissing means no credential exists for the requested MCP
	// server. Autonomous paths can classify this separately from store or
	// discovery failures and must not start OAuth discovery/login.
	ErrCredentialMissing = errors.New("MCP OAuth credential is missing")

	// ErrCredentialReauthRequired means a credential exists but cannot safely
	// be used (for example, it is legacy/unbound, bound to another URL, expired
	// without refresh metadata, or refresh failed). An explicit login is needed.
	ErrCredentialReauthRequired = errors.New("MCP OAuth credential requires explicit login")
)

// TokenEntry binds a bearer/refresh token to the concrete MCP server and the
// OAuth client/provider that issued it. The binding prevents a server rename
// or URL edit from reusing a bearer token against a different endpoint, while
// the persisted client metadata lets refresh reuse an RFC 7591 registration.
type TokenEntry struct {
	FormatVersion int         `json:"format_version"`
	ServerURL     string      `json:"server_url,omitempty"`
	ResourceURL   string      `json:"resource_url,omitempty"`
	Issuer        string      `json:"issuer,omitempty"`
	ClientID      string      `json:"client_id,omitempty"`
	AuthURL       string      `json:"authorization_endpoint,omitempty"`
	TokenURL      string      `json:"token_endpoint,omitempty"`
	Scopes        []string    `json:"scopes,omitempty"`
	Token         *auth.Token `json:"token"`

	legacy bool
}

// StoreError preserves the operation/path/cause for corrupt stores,
// permission failures, lock timeouts, and failed atomic persistence.
type StoreError struct {
	Op   string
	Path string
	Err  error
}

func (e *StoreError) Error() string {
	if e == nil {
		return "MCP OAuth token-store error"
	}
	return fmt.Sprintf("mcp oauth: token store %s %q: %v", e.Op, e.Path, e.Err)
}

func (e *StoreError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// TokenStore persists per-server OAuth entries below the private
// ~/.metis/.credentials directory.
type TokenStore struct {
	path       string
	legacyPath string
}

// NewTokenStore opens (lazily) the per-user token store.
func NewTokenStore() *TokenStore {
	credentialDir := auth.CredentialDirectory()
	path, legacyPath := "", ""
	if credentialDir != "" {
		path = filepath.Join(credentialDir, "mcp-oauth.json")
		legacyPath = filepath.Join(filepath.Dir(credentialDir), "mcp-oauth.json")
	}
	return &TokenStore{
		path:       path,
		legacyPath: legacyPath,
	}
}

func canonicalOAuthURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("mcp oauth: bad URL %q", raw)
	}
	if strings.Contains(raw, "#") {
		return "", fmt.Errorf("mcp oauth: URL must not contain a fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("mcp oauth: unsupported URL scheme %q", u.Scheme)
	}
	if u.Scheme == "http" && !isLoopbackOAuthHost(u.Hostname()) {
		return "", fmt.Errorf("mcp oauth: plain HTTP is allowed only for loopback URLs")
	}
	if u.User != nil {
		return "", fmt.Errorf("mcp oauth: credentials in URL are not allowed")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if u.RawQuery != "" {
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", fmt.Errorf("mcp oauth: invalid URL query: %w", err)
		}
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func isLoopbackOAuthHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func cloneTokenEntry(entry *TokenEntry) *TokenEntry {
	if entry == nil {
		return nil
	}
	clone := *entry
	clone.Scopes = append([]string(nil), entry.Scopes...)
	if entry.Token != nil {
		tokenClone := *entry.Token
		clone.Token = &tokenClone
	}
	return &clone
}

func (e *TokenEntry) boundTo(serverURL string) bool {
	return e != nil && !e.legacy && e.FormatVersion >= tokenEntryFormatVersion &&
		e.ServerURL != "" && e.ResourceURL != "" && e.Token != nil &&
		e.ServerURL == serverURL && e.ResourceURL == serverURL
}

func (e *TokenEntry) provider() (auth.OAuthProvider, error) {
	if e == nil || strings.TrimSpace(e.Issuer) == "" || strings.TrimSpace(e.ClientID) == "" ||
		strings.TrimSpace(e.AuthURL) == "" || strings.TrimSpace(e.TokenURL) == "" {
		return auth.OAuthProvider{}, fmt.Errorf("stored OAuth provider metadata is incomplete")
	}
	return auth.OAuthProvider{
		Name:            "mcp:" + e.Issuer,
		AuthURL:         e.AuthURL,
		TokenURL:        e.TokenURL,
		ClientID:        e.ClientID,
		Scopes:          append([]string(nil), e.Scopes...),
		UsePKCE:         true,
		ResourceURL:     e.ResourceURL,
		HeaderTokenType: "Bearer",
	}, nil
}

// GetEntry returns a defensive copy of the stored entry. A nil entry with a
// nil error means no record exists. Unlike Get, storage errors are preserved.
func (s *TokenStore) GetEntry(server string) (*TokenEntry, error) {
	var result *TokenEntry
	err := withTokenStoreLock(s.path, tokenStoreLockTimeout, func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		result = cloneTokenEntry(entries[server])
		return nil
	})
	if err != nil {
		return nil, &StoreError{Op: "read", Path: s.path, Err: err}
	}
	return result, nil
}

// GetWithError is the error-preserving counterpart of the legacy Get API.
func (s *TokenStore) GetWithError(server string) (*auth.Token, bool, error) {
	entry, err := s.GetEntry(server)
	if err != nil {
		return nil, false, err
	}
	if entry == nil || entry.Token == nil {
		return nil, false, nil
	}
	token := *entry.Token
	return &token, true, nil
}

// Get remains source-compatible with the original store API. New security-
// sensitive callers should use GetEntry/GetWithError so corruption or
// permission failures are not mistaken for a cache miss.
func (s *TokenStore) Get(server string) (*auth.Token, bool) {
	token, ok, err := s.GetWithError(server)
	if err != nil {
		return nil, false
	}
	return token, ok
}

// Put remains source-compatible, but deliberately creates an unbound record.
// Such records are readable through Get and fail closed in EnsureToken. OAuth
// login/refresh paths use PutEntry with a canonical server/resource binding.
func (s *TokenStore) Put(server string, token *auth.Token) error {
	return s.PutEntry(server, &TokenEntry{Token: token})
}

// PutEntry atomically persists a bound OAuth entry while preserving all other
// server records written by concurrently running CLI/Desktop processes.
func (s *TokenStore) PutEntry(server string, entry *TokenEntry) error {
	if strings.TrimSpace(server) == "" {
		return fmt.Errorf("mcp oauth: empty token-store server key")
	}
	var err error
	entry, err = normalizeTokenEntry(entry)
	if err != nil {
		return err
	}

	err = withTokenStoreLock(s.path, tokenStoreLockTimeout, func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		entries[server] = entry
		return s.writeUnlocked(entries)
	})
	if err != nil {
		return &StoreError{Op: "write", Path: s.path, Err: err}
	}
	return nil
}

func normalizeTokenEntry(entry *TokenEntry) (*TokenEntry, error) {
	if entry == nil || entry.Token == nil {
		return nil, fmt.Errorf("mcp oauth: nil token entry")
	}
	entry = cloneTokenEntry(entry)
	entry.FormatVersion = tokenEntryFormatVersion
	for label, value := range map[string]*string{
		"server": &entry.ServerURL, "resource": &entry.ResourceURL,
		"issuer": &entry.Issuer, "authorization endpoint": &entry.AuthURL,
		"token endpoint": &entry.TokenURL,
	} {
		if strings.TrimSpace(*value) == "" {
			continue
		}
		canonical, err := canonicalOAuthURL(*value)
		if err != nil {
			return nil, fmt.Errorf("mcp oauth: invalid %s URL: %w", label, err)
		}
		*value = canonical
	}
	if entry.ServerURL != "" && entry.ResourceURL != "" && entry.ServerURL != entry.ResourceURL {
		return nil, fmt.Errorf(
			"mcp oauth: resource URL %q does not match bound server URL %q",
			entry.ResourceURL, entry.ServerURL,
		)
	}
	return entry, nil
}

func tokenEntriesEqual(a, b *TokenEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.FormatVersion != b.FormatVersion || a.ServerURL != b.ServerURL ||
		a.ResourceURL != b.ResourceURL || a.Issuer != b.Issuer ||
		a.ClientID != b.ClientID || a.AuthURL != b.AuthURL ||
		a.TokenURL != b.TokenURL || a.legacy != b.legacy ||
		len(a.Scopes) != len(b.Scopes) || (a.Token == nil) != (b.Token == nil) {
		return false
	}
	for i := range a.Scopes {
		if a.Scopes[i] != b.Scopes[i] {
			return false
		}
	}
	if a.Token == nil {
		return true
	}
	return a.Token.AccessToken == b.Token.AccessToken &&
		a.Token.RefreshToken == b.Token.RefreshToken &&
		a.Token.ExpiresAt.Equal(b.Token.ExpiresAt) &&
		a.Token.Scope == b.Token.Scope &&
		a.Token.TokenType == b.Token.TokenType
}

// compareAndSwapEntry commits a refreshed credential only while the exact
// record used to start the network refresh remains current. It returns the
// winning record: replacement on success, a concurrent login's record on a
// conflict, or nil when a concurrent logout removed it.
func (s *TokenStore) compareAndSwapEntry(server string, expected, replacement *TokenEntry) (*TokenEntry, error) {
	if strings.TrimSpace(server) == "" {
		return nil, fmt.Errorf("mcp oauth: empty token-store server key")
	}
	if expected == nil || expected.Token == nil {
		return nil, fmt.Errorf("mcp oauth: nil expected token entry")
	}
	replacement, err := normalizeTokenEntry(replacement)
	if err != nil {
		return nil, err
	}
	var resolved *TokenEntry
	err = withTokenStoreLock(s.path, tokenStoreLockTimeout, func() error {
		entries, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		current := entries[server]
		if !tokenEntriesEqual(current, expected) {
			resolved = cloneTokenEntry(current)
			return nil
		}
		entries[server] = replacement
		if err := s.writeUnlocked(entries); err != nil {
			return err
		}
		resolved = cloneTokenEntry(replacement)
		return nil
	})
	if err != nil {
		return nil, &StoreError{Op: "compare-and-swap refresh", Path: s.path, Err: err}
	}
	return resolved, nil
}

func (s *TokenStore) loadUnlocked() (map[string]*TokenEntry, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, errors.New("MCP OAuth credential directory is unavailable")
	}
	entries, found, err := s.loadPathUnlocked(s.path)
	if err != nil {
		return nil, err
	}
	if s.legacyPath == "" || filepath.Clean(s.legacyPath) == filepath.Clean(s.path) {
		return entries, nil
	}
	legacy, legacyFound, err := s.loadPathUnlocked(s.legacyPath)
	if err != nil {
		return nil, err
	}
	if !legacyFound {
		return entries, nil
	}
	if !found {
		entries = legacy
	} else {
		// Canonical records win conflicts, but preserve servers only known to an
		// older concurrently installed client.
		for server, entry := range legacy {
			if _, exists := entries[server]; !exists {
				entries[server] = entry
			}
		}
	}
	if err := s.writeUnlocked(entries); err != nil {
		return nil, fmt.Errorf("migrate legacy MCP OAuth token store: %w", err)
	}
	if err := removeLegacyTokenStore(s.legacyPath); err != nil {
		return nil, fmt.Errorf("remove migrated legacy MCP OAuth token store: %w", err)
	}
	return entries, nil
}

func (s *TokenStore) loadPathUnlocked(storePath string) (map[string]*TokenEntry, bool, error) {
	entries := map[string]*TokenEntry{}
	file, found, err := openTokenStoreFile(storePath, tokenStoreMaxJSONBytes)
	if err != nil || !found {
		return entries, found, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, tokenStoreMaxJSONBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read token store: %w", err)
	}
	if len(data) > tokenStoreMaxJSONBytes {
		return nil, false, fmt.Errorf("token store is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var rawEntries map[string]json.RawMessage
	if err := decoder.Decode(&rawEntries); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, false, fmt.Errorf("decode token store: empty file")
		}
		return nil, false, fmt.Errorf("decode token store: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, fmt.Errorf("decode token store: trailing JSON value")
		}
		return nil, false, fmt.Errorf("decode token store: %w", err)
	}
	if rawEntries == nil {
		return nil, false, fmt.Errorf("decode token store: expected an object")
	}
	for server, raw := range rawEntries {
		var probe struct {
			FormatVersion int             `json:"format_version"`
			ServerURL     string          `json:"server_url"`
			Token         json.RawMessage `json:"token"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, false, fmt.Errorf("decode token entry %q: %w", server, err)
		}
		if probe.FormatVersion != 0 || probe.ServerURL != "" || len(probe.Token) > 0 {
			var entry TokenEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return nil, false, fmt.Errorf("decode token entry %q: %w", server, err)
			}
			if entry.Token == nil {
				return nil, false, fmt.Errorf("decode token entry %q: missing token", server)
			}
			entries[server] = &entry
			continue
		}

		// v1 compatibility: the original file mapped server names directly to
		// auth.Token. It remains inspectable through Get but has no URL/client
		// binding, so EnsureToken requires an explicit login before use.
		var token auth.Token
		if err := json.Unmarshal(raw, &token); err != nil {
			return nil, false, fmt.Errorf("decode legacy token entry %q: %w", server, err)
		}
		entries[server] = &TokenEntry{Token: &token, legacy: true}
	}
	return entries, true, nil
}

func removeLegacyTokenStore(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("refusing to remove symlinked or non-regular legacy MCP OAuth token store")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncTokenStoreDir(filepath.Dir(path))
}

func (s *TokenStore) writeUnlocked(entries map[string]*TokenEntry) error {
	bytes, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token store: %w", err)
	}
	if len(bytes) > tokenStoreMaxJSONBytes {
		return fmt.Errorf("encoded token store is too large")
	}
	dir := filepath.Dir(s.path)
	if err := ensurePrivateTokenStoreDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mcp-oauth-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	closeOnError := func(cause error) error {
		return errors.Join(cause, tmp.Close())
	}
	if err := tmp.Chmod(0o600); err != nil {
		return closeOnError(err)
	}
	if err := secureTokenStoreFile(tmpPath); err != nil {
		return closeOnError(fmt.Errorf("secure temporary token store: %w", err))
	}
	if _, err := tmp.Write(bytes); err != nil {
		return closeOnError(err)
	}
	if err := tmp.Sync(); err != nil {
		return closeOnError(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if info, err := os.Lstat(s.path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink token store")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("token store is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensurePrivateTokenStoreDir(dir); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	if err := secureTokenStoreFile(s.path); err != nil {
		return err
	}
	return syncTokenStoreDir(dir)
}
