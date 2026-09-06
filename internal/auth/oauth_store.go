package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	oauthStoreFormatVersion = 1
	oauthRefreshSkew        = 5 * time.Minute
	oauthStoreLockTimeout   = 2 * time.Second
	oauthRefreshLockTimeout = 30 * time.Second
)

var ErrOAuthCredentialExpired = errors.New("OAuth credential expired; sign in again")

// OAuthCredential is a complete, refreshable LLM credential. It deliberately
// lives in llm-oauth.json rather than auth.json: older METIS binaries know only
// the API-key schema and would otherwise discard refresh metadata on rewrite.
type OAuthCredential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
}

type oauthStoreFile struct {
	FormatVersion int                        `json:"format_version"`
	Credentials   map[string]OAuthCredential `json:"credentials"`
}

// OAuthStoreError preserves useful filesystem diagnostics without ever
// including the credential payload.
type OAuthStoreError struct {
	Op   string
	Path string
	Err  error
}

func (e *OAuthStoreError) Error() string {
	if e == nil {
		return "LLM OAuth credential-store error"
	}
	return fmt.Sprintf("llm oauth: credential store %s %q: %v", e.Op, e.Path, e.Err)
}

func (e *OAuthStoreError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func cloneOAuthCredential(credential *OAuthCredential) *OAuthCredential {
	if credential == nil {
		return nil
	}
	clone := *credential
	return &clone
}

func validateOAuthProviderID(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "", errors.New("llm oauth: provider required")
	}
	if len(provider) > 128 || strings.ContainsAny(provider, "\\/\x00\r\n\t") {
		return "", errors.New("llm oauth: invalid provider id")
	}
	return canonicalOAuthProviderID(provider), nil
}

func canonicalOAuthProviderID(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "anthropic-claudeai":
		return "anthropic"
	case "google":
		return "gemini"
	}
	return provider
}

// CanonicalProviderID normalizes a credential namespace for command-layer
// lookup and display. Storage mutations remain authoritative and also remove
// deprecated raw aliases left by older releases.
func CanonicalProviderID(provider string) string {
	return canonicalOAuthProviderID(provider)
}

func validateOAuthCredential(credential OAuthCredential) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("llm oauth: access token required")
	}
	fields := []struct {
		value string
		limit int
	}{
		{credential.AccessToken, 512 << 10},
		{credential.RefreshToken, 512 << 10},
		{credential.TokenType, 1 << 10},
		{credential.Scope, 64 << 10},
		{credential.AccountID, 4 << 10},
	}
	total := 0
	for _, field := range fields {
		total += len(field.value)
		if len(field.value) > field.limit || strings.ContainsAny(field.value, "\x00\r\n") {
			return errors.New("llm oauth: invalid credential field")
		}
	}
	if total > oauthMaxJSONResponseBytes-(64<<10) {
		return errors.New("llm oauth: credential is too large")
	}
	return nil
}

// GetOAuth returns a defensive copy. A nil credential and nil error means the
// provider has no OAuth login.
func GetOAuth(provider string) (*OAuthCredential, error) {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return nil, err
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return nil, err
	}
	path := layout.oauth
	var result *OAuthCredential
	err = withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		if credential, ok := store.Credentials[provider]; ok {
			result = cloneOAuthCredential(&credential)
		}
		return nil
	})
	if err != nil {
		return nil, &OAuthStoreError{Op: "read", Path: path, Err: err}
	}
	return result, nil
}

// PutOAuth performs a locked read-modify-write so concurrent CLI/Desktop
// processes cannot lose another provider's credential.
func PutOAuth(provider string, credential OAuthCredential) error {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return err
	}
	if err := validateOAuthCredential(credential); err != nil {
		return err
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	path := layout.oauth
	err = withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		store.Credentials[provider] = credential
		return writeOAuthStoreUnlocked(path, store)
	})
	if err != nil {
		return &OAuthStoreError{Op: "write", Path: path, Err: err}
	}
	return nil
}

func RemoveOAuth(provider string) error {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return err
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	path := layout.oauth
	err = withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		if _, ok := store.Credentials[provider]; !ok {
			return nil
		}
		delete(store.Credentials, provider)
		return writeOAuthStoreUnlocked(path, store)
	})
	if err != nil {
		return &OAuthStoreError{Op: "remove", Path: path, Err: err}
	}
	return nil
}

func ListOAuth() ([]string, error) {
	layout, err := currentCredentialLayout()
	if err != nil {
		return nil, err
	}
	path := layout.oauth
	var providers []string
	err = withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		providers = make([]string, 0, len(store.Credentials))
		for provider := range store.Credentials {
			providers = append(providers, provider)
		}
		sort.Strings(providers)
		return nil
	})
	if err != nil {
		return nil, &OAuthStoreError{Op: "list", Path: path, Err: err}
	}
	return providers, nil
}

func HasOAuth(provider string) (bool, error) {
	credential, err := GetOAuth(provider)
	return credential != nil, err
}

func loadOAuthStoreUnlocked(path string) (*oauthStoreFile, error) {
	store, found, err := loadOAuthStoreFileUnlocked(path)
	if err != nil {
		return nil, err
	}
	layout, layoutErr := currentCredentialLayout()
	if layoutErr != nil || filepath.Clean(path) != filepath.Clean(layout.oauth) {
		return store, layoutErr
	}
	legacy, legacyFound, err := loadOAuthStoreFileUnlocked(layout.legacyOAuth)
	if err != nil {
		return nil, err
	}
	if found {
		if !legacyFound {
			return store, nil
		}
		changed := false
		for provider, credential := range legacy.Credentials {
			if _, exists := store.Credentials[provider]; !exists {
				store.Credentials[provider] = credential
				changed = true
			}
		}
		if changed {
			if err := writeOAuthStoreUnlocked(path, store); err != nil {
				return nil, fmt.Errorf("merge legacy OAuth credential store: %w", err)
			}
		}
		if err := removeLegacyCredentialFile(layout.legacyOAuth); err != nil {
			return nil, fmt.Errorf("remove merged legacy OAuth credential store: %w", err)
		}
		return store, nil
	}
	if !legacyFound {
		return store, nil
	}
	if err := writeOAuthStoreUnlocked(path, legacy); err != nil {
		return nil, fmt.Errorf("migrate legacy OAuth credential store: %w", err)
	}
	if err := removeLegacyCredentialFile(layout.legacyOAuth); err != nil {
		return nil, fmt.Errorf("remove migrated legacy OAuth credential store: %w", err)
	}
	return legacy, nil
}

func loadOAuthStoreFileUnlocked(path string) (*oauthStoreFile, bool, error) {
	empty := &oauthStoreFile{FormatVersion: oauthStoreFormatVersion, Credentials: map[string]OAuthCredential{}}
	file, found, err := openCredentialStoreFile(path, oauthMaxJSONResponseBytes, false)
	if err != nil || !found {
		return empty, found, err
	}
	defer file.Close()

	decoder := json.NewDecoder(io.LimitReader(file, oauthMaxJSONResponseBytes+1))
	decoder.DisallowUnknownFields()
	var store oauthStoreFile
	if err := decoder.Decode(&store); err != nil {
		return nil, false, fmt.Errorf("decode credential store: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, errors.New("decode credential store: trailing JSON value")
		}
		return nil, false, fmt.Errorf("decode credential store: %w", err)
	}
	if store.FormatVersion != oauthStoreFormatVersion {
		return nil, false, fmt.Errorf("unsupported credential store format version %d", store.FormatVersion)
	}
	if store.Credentials == nil {
		return nil, false, errors.New("decode credential store: credentials must be an object")
	}
	for provider, credential := range store.Credentials {
		if _, err := validateOAuthProviderID(provider); err != nil {
			return nil, false, fmt.Errorf("decode credential store: invalid provider id")
		}
		if err := validateOAuthCredential(credential); err != nil {
			return nil, false, fmt.Errorf("decode credential store: invalid credential for %q", provider)
		}
	}
	// Older releases could persist aliases as literal map keys. Normalize them
	// while the file is in memory so canonical lookups and logout work before
	// the next write. If both forms exist, the explicit canonical entry wins
	// deterministically instead of depending on randomized map iteration.
	canonicalCredentials := make(map[string]OAuthCredential, len(store.Credentials))
	for provider, credential := range store.Credentials {
		canonical := canonicalOAuthProviderID(provider)
		if provider == canonical {
			canonicalCredentials[canonical] = credential
		}
	}
	for provider, credential := range store.Credentials {
		canonical := canonicalOAuthProviderID(provider)
		if _, exists := canonicalCredentials[canonical]; !exists {
			canonicalCredentials[canonical] = credential
		}
	}
	store.Credentials = canonicalCredentials
	return &store, true, nil
}

func writeOAuthStoreUnlocked(path string, store *oauthStoreFile) error {
	if store == nil {
		return errors.New("nil credential store")
	}
	store.FormatVersion = oauthStoreFormatVersion
	if store.Credentials == nil {
		store.Credentials = map[string]OAuthCredential{}
	}
	for provider, credential := range store.Credentials {
		if _, err := validateOAuthProviderID(provider); err != nil {
			return errors.New("credential store contains an invalid provider id")
		}
		if err := validateOAuthCredential(credential); err != nil {
			return fmt.Errorf("credential store contains an invalid credential for %q", provider)
		}
	}
	payload, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credential store: %w", err)
	}
	payload = append(payload, '\n')
	if len(payload) > oauthMaxJSONResponseBytes {
		return errors.New("encoded credential store is too large")
	}

	dir := filepath.Dir(path)
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".llm-oauth-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	closeOnError := func(cause error) error { return errors.Join(cause, tmp.Close()) }
	if err := tmp.Chmod(0o600); err != nil {
		return closeOnError(err)
	}
	if err := secureOAuthStoreFile(tmpPath); err != nil {
		return closeOnError(fmt.Errorf("secure temporary credential store: %w", err))
	}
	if _, err := tmp.Write(payload); err != nil {
		return closeOnError(err)
	}
	if err := tmp.Sync(); err != nil {
		return closeOnError(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing symlink credential store")
		}
		if !info.Mode().IsRegular() {
			return errors.New("credential store is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	if err := replaceOAuthStoreFile(tmpPath, path); err != nil {
		return err
	}
	if err := secureOAuthStoreFile(path); err != nil {
		return committedCredentialStoreWriteError(err)
	}
	if err := syncOAuthStoreDir(dir); err != nil {
		return committedCredentialStoreWriteError(err)
	}
	return nil
}

// ResolveOAuthCredential returns a usable credential and refreshes it once
// when it is within five minutes of expiry. Refresh is serialized per provider
// in this process and across CLI/Desktop processes, then double-checked after
// acquiring the lease.
func ResolveOAuthCredential(ctx context.Context, provider string) (*OAuthCredential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return nil, err
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return nil, err
	}
	storePath := layout.oauth
	credential, err := getOAuthAtPath(provider, storePath)
	if err != nil || credential == nil || !oauthCredentialNeedsRefresh(credential, time.Now()) {
		return credential, err
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return nil, ErrOAuthCredentialExpired
	}

	var resolved *OAuthCredential
	err = withOAuthRefreshLease(ctx, storePath, provider, func() (bool, error) {
		current, err := getOAuthAtPath(provider, storePath)
		if err != nil {
			return false, err
		}
		if current == nil {
			resolved = nil
			return false, nil
		}
		if !oauthCredentialNeedsRefresh(current, time.Now()) {
			resolved = current
			return false, nil
		}
		if strings.TrimSpace(current.RefreshToken) == "" {
			return false, ErrOAuthCredentialExpired
		}
		config, ok := KnownProviders[provider]
		if !ok {
			return false, fmt.Errorf("llm oauth: provider %q cannot refresh; sign in again", provider)
		}
		refreshedToken, err := RefreshTokenContext(ctx, config, current.RefreshToken)
		if err != nil {
			// Deliberately do not mutate the store on refresh failure.
			markFailure := ctx.Err() == nil
			return markFailure, fmt.Errorf("llm oauth: refresh failed; sign in again: %w", err)
		}
		// OAuth token endpoints are allowed to omit refresh_token when the
		// existing token remains valid. Subscription providers require a
		// refreshable credential, so inherit it before validation rather than
		// after credentialFromToken has already rejected the response.
		if refreshedToken != nil && strings.TrimSpace(refreshedToken.RefreshToken) == "" {
			refreshedToken.RefreshToken = current.RefreshToken
		}
		refreshed, err := credentialFromToken(provider, refreshedToken)
		if err != nil {
			// A syntactically successful token endpoint response that cannot form
			// a safe refreshable credential is still a provider-side refresh
			// failure. Coalesce concurrent waiters instead of replaying it.
			return true, err
		}
		if refreshed.RefreshToken == "" {
			refreshed.RefreshToken = current.RefreshToken
		}
		if refreshed.TokenType == "" {
			refreshed.TokenType = current.TokenType
		}
		if refreshed.Scope == "" {
			refreshed.Scope = current.Scope
		}
		if refreshed.AccountID == "" {
			refreshed.AccountID = current.AccountID
		}
		resolved, err = compareAndSwapOAuthAtPath(provider, *current, *refreshed, storePath)
		if err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return cloneOAuthCredential(resolved), nil
}

// compareAndSwapOAuthAtPath commits a refresh only if the credential has not
// changed while the network request was in flight. A concurrent logout must
// stay logged out, and a concurrent new login must never be overwritten by a
// stale refresh response.
func compareAndSwapOAuthAtPath(provider string, expected, replacement OAuthCredential, path string) (*OAuthCredential, error) {
	if err := validateOAuthCredential(replacement); err != nil {
		return nil, err
	}
	var resolved *OAuthCredential
	err := withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		current, ok := store.Credentials[provider]
		if !ok {
			return nil
		}
		if current != expected {
			resolved = cloneOAuthCredential(&current)
			return nil
		}
		store.Credentials[provider] = replacement
		if err := writeOAuthStoreUnlocked(path, store); err != nil {
			return err
		}
		resolved = cloneOAuthCredential(&replacement)
		return nil
	})
	if err != nil {
		return nil, &OAuthStoreError{Op: "compare-and-swap refresh", Path: path, Err: err}
	}
	return resolved, nil
}

func getOAuthAtPath(provider, path string) (*OAuthCredential, error) {
	var result *OAuthCredential
	err := withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		if credential, ok := store.Credentials[provider]; ok {
			result = cloneOAuthCredential(&credential)
		}
		return nil
	})
	if err != nil {
		return nil, &OAuthStoreError{Op: "read", Path: path, Err: err}
	}
	return result, nil
}

func putOAuthAtPath(provider string, credential OAuthCredential, path string) error {
	if err := validateOAuthCredential(credential); err != nil {
		return err
	}
	err := withOAuthStoreLock(path, oauthStoreLockTimeout, func() error {
		store, err := loadOAuthStoreUnlocked(path)
		if err != nil {
			return err
		}
		store.Credentials[provider] = credential
		return writeOAuthStoreUnlocked(path, store)
	})
	if err != nil {
		return &OAuthStoreError{Op: "write", Path: path, Err: err}
	}
	return nil
}

func oauthCredentialNeedsRefresh(credential *OAuthCredential, now time.Time) bool {
	return credential != nil && !credential.ExpiresAt.IsZero() && !now.Add(oauthRefreshSkew).Before(credential.ExpiresAt)
}
