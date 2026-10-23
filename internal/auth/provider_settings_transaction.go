package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// These seams cover commit attempts only. Restoration deliberately uses the
// underlying secure writer, so a failed commit writer cannot disable rollback.
var (
	providerSettingsWriteAuthStore  = writeAuthStoreUnlocked
	providerSettingsWriteOAuthStore = writeOAuthStoreUnlocked
	providerSettingsRemoveLegacy    = removeLegacyCredentialFile
	providerSettingsWithStoreLock   = withOAuthStoreLock
)

// ProviderSettingsCredentials is an isolated credential view belonging to one
// provider-settings transaction. Its contents must never be logged. Read methods
// remain usable after the transaction; mutations are valid only inside fn.
// It is not safe for concurrent use.
type ProviderSettingsCredentials struct {
	apiKeys     File
	oauth       *oauthStoreFile
	savedAPI    map[string]bool
	fingerprint string
	active      bool
	dirty       bool
}

// Fingerprint identifies API, OAuth and both legacy files, including their
// absence and exact contents, without returning any credential payload. Staged
// mutations retain the entry fingerprint; a successful commit publishes the new
// fingerprint before WithProviderSettingsCredentials returns.
func (c *ProviderSettingsCredentials) Fingerprint() string {
	if c == nil {
		return ""
	}
	return c.fingerprint
}

// String and GoString prevent ordinary diagnostic formatting from exposing
// staged secrets through the otherwise opaque transaction value.
func (c *ProviderSettingsCredentials) String() string {
	return "ProviderSettingsCredentials{fingerprint:" + c.Fingerprint() + "}"
}

func (c *ProviderSettingsCredentials) GoString() string { return c.String() }

// ManagedAPIKey returns a defensive copy. An explicit canonical entry wins over
// deprecated aliases; aliases are otherwise selected in deterministic order.
func (c *ProviderSettingsCredentials) ManagedAPIKey(provider string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	provider = canonicalOAuthProviderID(provider)
	if provider == "" || isNamespacedKey(provider) {
		return Entry{}, false
	}
	clone := func(entry Entry) (Entry, bool) {
		if entry.Type != "api" || strings.TrimSpace(entry.Key) == "" || isMisclassifiedOAuthEntry(provider, entry.Key) {
			return Entry{}, false
		}
		if entry.Endpoint != nil {
			binding := *entry.Endpoint
			entry.Endpoint = &binding
		}
		return entry, true
	}
	if entry, exists := c.apiKeys[provider]; exists {
		return clone(entry)
	}
	keys := make([]string, 0, len(c.apiKeys))
	for key := range c.apiKeys {
		if canonicalOAuthProviderID(key) == provider {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if entry, valid := clone(c.apiKeys[key]); valid {
			return entry, true
		}
	}
	return Entry{}, false
}

func (c *ProviderSettingsCredentials) HasOAuth(provider string) bool {
	if c == nil || c.oauth == nil {
		return false
	}
	_, exists := c.oauth.Credentials[canonicalOAuthProviderID(provider)]
	return exists
}

// OAuthCredentialReady mirrors the runtime's stored OAuth readiness check
// without loading, migrating or refreshing a credential. Codex needs complete
// refresh/account metadata; an expired, refreshable record remains ready.
func (c *ProviderSettingsCredentials) OAuthCredentialReady(provider string) bool {
	provider = canonicalOAuthProviderID(provider)
	if !c.HasOAuth(provider) {
		return false
	}
	if provider != openAICodexProviderID {
		return true
	}
	credential := c.oauth.Credentials[provider]
	return strings.TrimSpace(credential.AccessToken) != "" &&
		strings.TrimSpace(credential.RefreshToken) != "" &&
		strings.TrimSpace(credential.AccountID) != "" && !credential.ExpiresAt.IsZero()
}

// ProbeAPIKeyForEndpoint checks a saved key's endpoint authorization without
// returning its secret or invoking auth.Load's legacy cleanup. Historical OAuth
// tokens misclassified as API keys and blank keys are unavailable, but remain
// saved records for membership and explicit repair/deletion.
func (c *ProviderSettingsCredentials) ProbeAPIKeyForEndpoint(provider, transport, baseURL string, allowLegacyOfficial bool) (bool, error) {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, nil
	}
	entry, exists := c.apiKeys[provider]
	if !exists {
		var aliases []string
		for stored := range c.apiKeys {
			if canonicalOAuthProviderID(stored) == provider {
				aliases = append(aliases, stored)
			}
		}
		sort.Strings(aliases)
		if len(aliases) > 0 {
			entry, exists = c.apiKeys[aliases[0]], true
		}
	}
	if !exists || isMisclassifiedOAuthEntry(provider, entry.Key) {
		return false, nil
	}
	if isNamespacedKey(provider) || entry.Type != "api" {
		return false, errors.New("auth: stored credential is not an LLM API key")
	}
	if strings.TrimSpace(entry.Key) == "" {
		return false, nil
	}
	binding, err := NormalizeEndpointBinding(provider, transport, baseURL)
	if err != nil {
		return false, err
	}
	if entry.Endpoint == nil {
		if allowLegacyOfficial {
			return true, nil
		}
		return false, fmt.Errorf("%w for provider %q; run `metis login %s` again", ErrEndpointBindingRequired, binding.Provider, binding.Provider)
	}
	stored, err := NormalizeEndpointBinding(entry.Endpoint.Provider, entry.Endpoint.Transport, entry.Endpoint.BaseURL)
	if err != nil {
		return false, fmt.Errorf("%w for provider %q: invalid stored endpoint metadata: %v", ErrEndpointBindingMismatch, binding.Provider, err)
	}
	if stored != binding {
		return false, fmt.Errorf("%w for provider %q; run `metis login %s` again", ErrEndpointBindingMismatch, binding.Provider, binding.Provider)
	}
	return true, nil
}

// HasSavedProvider reports membership, not whether a credential is usable. The
// API presence index includes unsafe historical OAuth-in-API entries, which
// ManagedAPIKey refuses to return. Expired OAuth and legacy aliases likewise
// remain registered so callers can offer repair or deletion without exposing a
// secret. Structurally corrupt stores still fail the enclosing transaction.
func (c *ProviderSettingsCredentials) HasSavedProvider(provider string) bool {
	if c == nil {
		return false
	}
	provider = canonicalOAuthProviderID(provider)
	if provider == "" || isNamespacedKey(provider) {
		return false
	}
	return c.savedAPI[provider] || c.HasOAuth(provider)
}

// SetAPIKeyBound stages the same validation and exclusive login-method change
// as ActivateAPIKeyBound, without writing any file before fn succeeds.
func (c *ProviderSettingsCredentials) SetAPIKeyBound(provider, key, transport, baseURL string) error {
	if c == nil || !c.active {
		return errors.New("auth: provider settings transaction is not active")
	}
	binding, err := NormalizeEndpointBinding(provider, transport, baseURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("auth: key required")
	}
	if isMisclassifiedOAuthEntry(binding.Provider, key) {
		return errors.New("auth: refusing to activate an OAuth access token as an API key; run `metis login` with the OAuth method")
	}
	c.ClearAPIKey(binding.Provider)
	c.apiKeys[binding.Provider] = Entry{Type: "api", Key: key, Endpoint: &binding}
	c.savedAPI[binding.Provider] = true
	delete(c.oauth.Credentials, binding.Provider)
	c.dirty = true
	return nil
}

// ClearAPIKey removes all API-key aliases for provider, while retaining OAuth.
// It is a no-op outside the callback's active lifetime.
func (c *ProviderSettingsCredentials) ClearAPIKey(provider string) {
	if c == nil || !c.active {
		return
	}
	provider = canonicalOAuthProviderID(provider)
	if provider == "" || isNamespacedKey(provider) {
		return
	}
	for stored := range c.apiKeys {
		if canonicalOAuthProviderID(stored) == provider {
			delete(c.apiKeys, stored)
		}
	}
	delete(c.savedAPI, provider)
	c.dirty = true
}

// ClearProvider removes every saved login method for the canonical provider,
// including aliases from legacy stores. The normal commit migrates the other
// legacy records and retires both legacy files, preventing removed records from
// reappearing through a later loader. It is a no-op outside the active callback.
func (c *ProviderSettingsCredentials) ClearProvider(provider string) {
	if c == nil || !c.active {
		return
	}
	provider = canonicalOAuthProviderID(provider)
	if provider == "" || isNamespacedKey(provider) {
		return
	}
	c.ClearAPIKey(provider)
	for stored := range c.oauth.Credentials {
		if canonicalOAuthProviderID(stored) == provider {
			delete(c.oauth.Credentials, stored)
		}
	}
	c.dirty = true
}

type providerSettingsCredentialFile struct {
	path    string
	present bool
	data    []byte
}

// WithProviderSettingsCredentials holds the shared in-process and cross-process
// credential lock throughout fn and commit. Callers coordinating configuration
// must take their config lock first. fn must not call public auth functions
// (which would reacquire this non-reentrant lock), and must not retain a view for
// mutation. A read-only callback never rewrites or migrates credential files.
// A callback error performs no credential writes. Any commit error restores the
// previous API, OAuth and legacy file contents before returning the error.
func WithProviderSettingsCredentials(fn func(*ProviderSettingsCredentials) error) error {
	if fn == nil {
		return errors.New("auth: provider settings callback required")
	}
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	var committed *ProviderSettingsCredentials
	var original, committedFiles, touched []providerSettingsCredentialFile
	err = providerSettingsWithStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		before, err := readProviderSettingsCredentialFiles(layout)
		if err != nil {
			return err
		}
		original = before
		api, oauth, savedAPI, err := providerSettingsCredentialView(before)
		if err != nil {
			return err
		}
		c := &ProviderSettingsCredentials{apiKeys: api, oauth: oauth, savedAPI: savedAPI, fingerprint: providerSettingsCredentialFingerprint(before), active: true}
		defer func() { c.active = false }()
		if err := fn(c); err != nil {
			return err
		}
		c.active = false
		if !c.dirty {
			return nil
		}
		privateAPI, err := decodeProviderSettingsAPI(before[0])
		if err != nil {
			return err
		}
		privateOAuth, err := decodeProviderSettingsOAuth(before[1])
		if err != nil {
			return err
		}
		rollback := func(cause error) error {
			restoreErr := restoreProviderSettingsCredentialFiles(layout, touched)
			if restoreErr == nil {
				api, oauth, savedAPI, viewErr := providerSettingsCredentialView(before)
				if viewErr == nil {
					c.apiKeys, c.oauth, c.savedAPI = api, oauth, savedAPI
				}
				restoreErr = viewErr
			}
			return errors.Join(cause, restoreErr)
		}
		// Record each file before attempting its write: even a returned error
		// may happen after rename and therefore requires restoration.
		if before[2].present || !reflect.DeepEqual(privateAPI, c.apiKeys) {
			touched = append(touched, before[0])
			if err := providerSettingsWriteAuthStore(layout.auth, c.apiKeys); err != nil {
				return rollback(fmt.Errorf("commit API-key credentials: %w", err))
			}
		}
		if before[3].present || !reflect.DeepEqual(privateOAuth, c.oauth) {
			touched = append(touched, before[1])
			if err := providerSettingsWriteOAuthStore(layout.oauth, c.oauth); err != nil {
				return rollback(fmt.Errorf("commit OAuth credentials: %w", err))
			}
		}
		for _, legacy := range before[2:] {
			if legacy.present {
				touched = append(touched, legacy)
				if err := providerSettingsRemoveLegacy(legacy.path); err != nil {
					return rollback(fmt.Errorf("retire legacy credentials: %w", err))
				}
			}
		}
		after, err := readProviderSettingsCredentialFiles(layout)
		if err != nil {
			return rollback(fmt.Errorf("verify committed credentials: %w", err))
		}
		actualAPI, actualOAuth, actualSavedAPI, err := providerSettingsCredentialView(after)
		if err != nil {
			return rollback(fmt.Errorf("verify committed credentials: %w", err))
		}
		if after[2].present || after[3].present || !reflect.DeepEqual(actualAPI, c.apiKeys) || !reflect.DeepEqual(actualOAuth, c.oauth) {
			return rollback(errors.New("auth: committed provider credentials did not reach the requested state"))
		}
		c.savedAPI = actualSavedAPI
		c.fingerprint = providerSettingsCredentialFingerprint(after)
		committed, committedFiles = c, after
		return nil
	})
	if err == nil || committed == nil {
		return err
	}
	// The existing lock primitive may report unlock/close errors after fn has
	// committed. The config caller treats every error as an aborted save, so
	// reacquire the same auth lock and restore before propagating that error.
	// Compare all four files first: never overwrite a concurrent independent
	// login that won the lock in the interval after release.
	restoreErr := withOAuthStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		current, readErr := readProviderSettingsCredentialFiles(layout)
		if readErr != nil {
			return readErr
		}
		if !reflect.DeepEqual(current, committedFiles) {
			return errors.New("auth: credentials changed while recovering from a credential-lock release failure")
		}
		if restoreErr := restoreProviderSettingsCredentialFiles(layout, touched); restoreErr != nil {
			return restoreErr
		}
		afterRestore, readErr := readProviderSettingsCredentialFiles(layout)
		if readErr != nil {
			return readErr
		}
		if !reflect.DeepEqual(afterRestore, original) {
			return errors.New("auth: credential-lock failure recovery did not restore the original state")
		}
		api, oauth, savedAPI, viewErr := providerSettingsCredentialView(original)
		if viewErr != nil {
			return viewErr
		}
		committed.apiKeys, committed.oauth, committed.savedAPI = api, oauth, savedAPI
		committed.fingerprint = providerSettingsCredentialFingerprint(original)
		return nil
	})
	if restoreErr != nil {
		return errors.Join(err, fmt.Errorf("restore credentials after lock release failure: %w", restoreErr))
	}
	return err
}

func readProviderSettingsCredentialFiles(layout credentialLayout) ([]providerSettingsCredentialFile, error) {
	paths := []string{layout.auth, layout.oauth, layout.legacyAuth, layout.legacyOAuth}
	files := make([]providerSettingsCredentialFile, 0, len(paths))
	for i, path := range paths {
		limit := int64(authStoreMaxJSONBytes)
		if i%2 == 1 {
			limit = oauthMaxJSONResponseBytes
		}
		file, present, err := openCredentialStoreFile(path, limit, false)
		if err != nil {
			return nil, fmt.Errorf("read provider settings credentials: %w", err)
		}
		snapshot := providerSettingsCredentialFile{path: path, present: present}
		if present {
			snapshot.data, err = io.ReadAll(io.LimitReader(file, limit+1))
			err = errors.Join(err, file.Close())
			if err != nil || int64(len(snapshot.data)) > limit {
				return nil, errors.New("auth: unable to read bounded credential store")
			}
		}
		files = append(files, snapshot)
	}
	return files, nil
}

func providerSettingsCredentialFingerprint(files []providerSettingsCredentialFile) string {
	h := sha256.New()
	for i, file := range files {
		fmt.Fprintf(h, "%d:%t:%d:", i, file.present, len(file.data))
		_, _ = h.Write(file.data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func providerSettingsCredentialView(files []providerSettingsCredentialFile) (File, *oauthStoreFile, map[string]bool, error) {
	api, err := decodeProviderSettingsAPI(files[0])
	if err != nil {
		return nil, nil, nil, err
	}
	legacyAPI, err := decodeProviderSettingsAPI(files[2])
	if err != nil {
		return nil, nil, nil, err
	}
	savedAPI := make(map[string]bool, len(api)+len(legacyAPI))
	for _, stored := range []File{api, legacyAPI} {
		for provider := range stored {
			canonical := canonicalOAuthProviderID(provider)
			if canonical != "" && !isNamespacedKey(canonical) {
				savedAPI[canonical] = true
			}
		}
	}
	// Keep unusable records in the transactional view so editing another
	// provider cannot silently delete them. Only ManagedAPIKey filters unsafe
	// credentials; explicit ClearAPIKey/ClearProvider own their removal.
	for provider, entry := range legacyAPI {
		if _, exists := api[provider]; !exists {
			api[provider] = entry
		}
	}
	oauth, err := decodeProviderSettingsOAuth(files[1])
	if err != nil {
		return nil, nil, nil, err
	}
	legacyOAuth, err := decodeProviderSettingsOAuth(files[3])
	if err != nil {
		return nil, nil, nil, err
	}
	for provider, credential := range legacyOAuth.Credentials {
		if _, exists := oauth.Credentials[provider]; !exists {
			oauth.Credentials[provider] = credential
		}
	}
	return api, oauth, savedAPI, nil
}

func decodeProviderSettingsAPI(file providerSettingsCredentialFile) (File, error) {
	result := File{}
	if !file.present || len(bytes.TrimSpace(file.data)) == 0 {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(file.data))
	if err := decoder.Decode(&result); err != nil {
		return nil, errors.New("auth: invalid API-key credential store JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("auth: trailing API-key credential store JSON")
	}
	if result == nil {
		result = File{}
	}
	return result, nil
}

func decodeProviderSettingsOAuth(file providerSettingsCredentialFile) (*oauthStoreFile, error) {
	result := &oauthStoreFile{FormatVersion: oauthStoreFormatVersion, Credentials: map[string]OAuthCredential{}}
	if !file.present {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(file.data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return nil, errors.New("auth: invalid OAuth credential store JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("auth: trailing OAuth credential store JSON")
	}
	if result.FormatVersion != oauthStoreFormatVersion || result.Credentials == nil {
		return nil, errors.New("auth: unsupported OAuth credential store")
	}
	keys := make([]string, 0, len(result.Credentials))
	for provider, credential := range result.Credentials {
		if _, err := validateOAuthProviderID(provider); err != nil {
			return nil, errors.New("auth: invalid OAuth provider id")
		}
		if err := validateOAuthCredential(credential); err != nil {
			return nil, errors.New("auth: invalid OAuth credential")
		}
		keys = append(keys, provider)
	}
	sort.Strings(keys)
	canonical := make(map[string]OAuthCredential, len(keys))
	for _, provider := range keys {
		if provider == canonicalOAuthProviderID(provider) {
			canonical[provider] = result.Credentials[provider]
		}
	}
	for _, provider := range keys {
		id := canonicalOAuthProviderID(provider)
		if _, exists := canonical[id]; !exists {
			canonical[id] = result.Credentials[provider]
		}
	}
	result.Credentials = canonical
	return result, nil
}

func restoreProviderSettingsCredentialFile(layout credentialLayout, snapshot providerSettingsCredentialFile) error {
	if err := ensurePrivateOAuthStoreDir(layout.dir); err != nil {
		return err
	}
	if !snapshot.present {
		return removeLegacyCredentialFile(snapshot.path)
	}
	dir := filepath.Dir(snapshot.path)
	tmp, err := os.CreateTemp(dir, ".provider-settings-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	closeOnError := func(err error) error { return errors.Join(err, tmp.Close()) }
	if err := tmp.Chmod(0o600); err != nil {
		return closeOnError(err)
	}
	if err := secureOAuthStoreFile(tmpPath); err != nil {
		return closeOnError(err)
	}
	if _, err := tmp.Write(snapshot.data); err != nil {
		return closeOnError(err)
	}
	if err := tmp.Sync(); err != nil {
		return closeOnError(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(snapshot.path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("refusing to restore symlinked or non-regular credential store")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensurePrivateOAuthStoreDir(layout.dir); err != nil {
		return err
	}
	if err := replaceOAuthStoreFile(tmpPath, snapshot.path); err != nil {
		return err
	}
	if err := secureOAuthStoreFile(snapshot.path); err != nil {
		return err
	}
	return syncOAuthStoreDir(dir)
}

func restoreProviderSettingsCredentialFiles(layout credentialLayout, files []providerSettingsCredentialFile) error {
	var result error
	for i := len(files) - 1; i >= 0; i-- {
		if err := restoreProviderSettingsCredentialFile(layout, files[i]); err != nil {
			result = errors.Join(result, fmt.Errorf("restore credential file: %w", err))
		}
	}
	return result
}
