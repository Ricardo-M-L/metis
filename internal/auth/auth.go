// Package auth manages provider credentials persisted below
// ~/.metis/.credentials.
//
// The file format and 0o600 perm match opencode's auth.json:
//
//	{
//	  "anthropic": {"type": "api", "key": "sk-..."},
//	  "openai":    {"type": "api", "key": "sk-..."}
//	}
//
// This is a deliberately separate file from config.toml. config.toml is meant
// to be diffable / shareable; auth.json holds raw secrets and never should be.
// Keeping them split also lets `metis login` rewrite credentials atomically
// without touching unrelated config.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const authStoreMaxJSONBytes = 4 << 20

// File is the on-disk shape of ~/.metis/.credentials/auth.json.
// Keyed by provider id ("anthropic", "openai", "minimax", or any custom id).
type File map[string]Entry

// Entry is one provider's credential. `Type` is currently always "api"; it's
// kept as a discriminator so future flows (oauth, instance creds) slot in
// without a schema migration.
type Entry struct {
	Type     string           `json:"type"`
	Key      string           `json:"key"`
	Endpoint *EndpointBinding `json:"endpoint,omitempty"`
}

// Load reads auth.json. A missing file is NOT an error — it returns an empty
// map so callers can append-and-save without first checking existence.
func Load() (File, error) {
	layout, err := currentCredentialLayout()
	if err != nil {
		return nil, err
	}
	var result File
	err = withOAuthStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		var loadErr error
		result, loadErr = loadAuthStoreUnlocked(layout)
		return loadErr
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", layout.auth, err)
	}
	return result, nil
}

// Save writes auth.json atomically with 0o600 perms.
// Atomic = write to a sibling temp file then rename, so a crash mid-write
// can't leave a half-truncated credentials file.
func Save(f File) error {
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	return withOAuthStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		if err := writeAuthStoreUnlocked(layout.auth, f); err != nil {
			return fmt.Errorf("write %s: %w", layout.auth, err)
		}
		return removeLegacyCredentialFile(layout.legacyAuth)
	})
}

func loadAuthStoreUnlocked(layout credentialLayout) (File, error) {
	file, found, err := readAuthStoreFile(layout.auth)
	if err != nil {
		return nil, err
	}
	discarded := stripMisclassifiedOAuthEntries(file)
	legacy, legacyFound, err := readAuthStoreFile(layout.legacyAuth)
	if err != nil {
		return nil, err
	}
	discarded = append(discarded, stripMisclassifiedOAuthEntries(legacy)...)
	if found {
		if !legacyFound {
			if len(discarded) > 0 {
				if err := writeAuthStoreUnlocked(layout.auth, file); err != nil {
					return nil, fmt.Errorf("remove misclassified OAuth credential: %w", err)
				}
				warnMisclassifiedOAuthEntries(discarded)
			}
			return file, nil
		}
		changed := len(discarded) > 0
		for provider, entry := range legacy {
			if _, exists := file[provider]; !exists {
				file[provider] = entry
				changed = true
			}
		}
		if changed {
			if err := writeAuthStoreUnlocked(layout.auth, file); err != nil {
				return nil, fmt.Errorf("merge legacy auth store: %w", err)
			}
		}
		if err := removeLegacyCredentialFile(layout.legacyAuth); err != nil {
			return nil, fmt.Errorf("remove merged legacy auth store: %w", err)
		}
		warnMisclassifiedOAuthEntries(discarded)
		return file, nil
	}
	if !legacyFound {
		return File{}, nil
	}
	// Migrate only after the legacy file was parsed and secured. Writing the
	// canonical copy first makes a crash leave at least one usable credential
	// store; removing the old copy afterwards avoids retaining duplicate secrets.
	if err := writeAuthStoreUnlocked(layout.auth, legacy); err != nil {
		return nil, fmt.Errorf("migrate legacy auth store: %w", err)
	}
	if err := removeLegacyCredentialFile(layout.legacyAuth); err != nil {
		return nil, fmt.Errorf("remove migrated legacy auth store: %w", err)
	}
	warnMisclassifiedOAuthEntries(discarded)
	return legacy, nil
}

// stripMisclassifiedOAuthEntries removes access tokens written by historical
// OAuth code into the API-key store. They must never be sent as x-api-key or
// to a same-named custom LLM endpoint. Anthropic's old store omitted the
// refresh token, so the safe recovery is an explicit fresh login rather than
// pretending the access token is a durable API key.
func stripMisclassifiedOAuthEntries(file File) []string {
	var discarded []string
	for provider, entry := range file {
		if !isMisclassifiedOAuthEntry(provider, entry.Key) {
			continue
		}
		delete(file, provider)
		discarded = append(discarded, strings.ToLower(strings.TrimSpace(provider)))
	}
	sort.Strings(discarded)
	return discarded
}

func isMisclassifiedOAuthEntry(provider, key string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	key = strings.ToLower(strings.TrimSpace(key))
	switch provider {
	case "anthropic", "anthropic-claudeai":
		return strings.HasPrefix(key, "sk-ant-oat")
	case "openai-codex":
		// This provider is OAuth-only; no value is a valid platform API key.
		return key != ""
	case "github":
		return strings.HasPrefix(key, "gho_")
	default:
		return false
	}
}

func warnMisclassifiedOAuthEntries(providers []string) {
	if len(providers) == 0 {
		return
	}
	providers = append([]string(nil), providers...)
	sort.Strings(providers)
	unique := providers[:0]
	for _, provider := range providers {
		if len(unique) == 0 || unique[len(unique)-1] != provider {
			unique = append(unique, provider)
		}
	}
	fmt.Fprintf(os.Stderr, "metis: discarded legacy OAuth access token(s) misclassified as API keys for %s; sign in again with `metis login`\n", strings.Join(unique, ", "))
}

func readAuthStoreFile(path string) (File, bool, error) {
	file, found, err := openCredentialStoreFile(path, authStoreMaxJSONBytes, true)
	if err != nil || !found {
		return File{}, found, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, authStoreMaxJSONBytes+1))
	var result File
	if err := decoder.Decode(&result); err != nil {
		if errors.Is(err, io.EOF) {
			return File{}, true, nil
		}
		return nil, false, fmt.Errorf("parse credential store: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, errors.New("parse credential store: trailing JSON value")
		}
		return nil, false, fmt.Errorf("parse credential store: %w", err)
	}
	if result == nil {
		result = File{}
	}
	return result, true, nil
}

func writeAuthStoreUnlocked(final string, f File) error {
	// Stable key order so the file diffs cleanly across runs.
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]Entry, len(f))
	for _, k := range keys {
		ordered[k] = f[k]
	}
	b, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(b) > authStoreMaxJSONBytes {
		return errors.New("encoded credential store is too large")
	}
	dir := filepath.Dir(final)
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth.json.*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // best-effort cleanup if rename fails

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := secureOAuthStoreFile(tmpPath); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure tempfile: %w", err)
	}
	// Flush the complete credential payload and its tightened metadata before
	// publishing the temp file with an atomic rename. Without this fsync, a
	// successful rename can still expose an empty or stale store after a crash.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile: %w", err)
	}
	if info, err := os.Lstat(final); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("refusing symlinked or non-regular credential store")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	if err := replaceOAuthStoreFile(tmpPath, final); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, final, err)
	}
	if err := secureOAuthStoreFile(final); err != nil {
		return committedCredentialStoreWriteError(err)
	}
	if err := syncOAuthStoreDir(dir); err != nil {
		return committedCredentialStoreWriteError(err)
	}
	return nil
}

func removeLegacyCredentialFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("refusing to remove symlinked or non-regular legacy credential store")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncOAuthStoreDir(filepath.Dir(path))
}

// Set stores a provider credential and persists immediately.
// Empty key/provider both error so callers don't accidentally write blanks.
func Set(provider, key string) error {
	if provider == "" {
		return errors.New("auth: provider required")
	}
	if key == "" {
		return errors.New("auth: key required")
	}
	if isMisclassifiedOAuthEntry(provider, key) {
		return errors.New("auth: refusing to store an OAuth access token as an API key; use the OAuth credential store or run `metis login`")
	}
	return updateAuthStore(func(f File) {
		f[provider] = Entry{Type: "api", Key: key}
	})
}

// Get returns the api key for the exact provider id. Empty string + nil error
// means missing, allowing callers to fall through to env / config.toml.
//
// Credentials are deliberately never guessed across provider ids. Wire-format
// compatibility does not imply endpoint identity: treating a MiniMax or Groq
// key as an Anthropic or OpenAI key can send it to the wrong vendor. Legacy
// alias entries remain listable/removable and can be migrated only after an
// explicit provider profile binds them to the intended endpoint.
func Get(provider string) (string, error) {
	f, err := Load()
	if err != nil {
		return "", err
	}
	if e, ok := f[provider]; ok {
		return e.Key, nil
	}
	return "", nil
}

// GetAPIKeyForEndpoint returns a managed API key only when it belongs to the
// requested provider endpoint. Unbound entries are accepted solely when the
// caller has established that this is a built-in provider's official endpoint;
// custom and third-party endpoints must be explicitly bound by a fresh login.
func GetAPIKeyForEndpoint(provider, transport, baseURL string, allowLegacyOfficial bool) (string, error) {
	provider, err := validateOAuthProviderID(provider)
	if err != nil {
		return "", err
	}
	f, err := Load()
	if err != nil {
		return "", err
	}
	entry, ok := f[provider]
	if !ok {
		return "", nil
	}
	if isNamespacedKey(provider) || entry.Type != "api" {
		return "", errors.New("auth: stored credential is not an LLM API key")
	}
	binding, err := NormalizeEndpointBinding(provider, transport, baseURL)
	if err != nil {
		return "", err
	}
	if entry.Endpoint == nil {
		if allowLegacyOfficial {
			return entry.Key, nil
		}
		return "", fmt.Errorf("%w for provider %q; run `metis login %s` again", ErrEndpointBindingRequired, binding.Provider, binding.Provider)
	}
	stored, err := NormalizeEndpointBinding(entry.Endpoint.Provider, entry.Endpoint.Transport, entry.Endpoint.BaseURL)
	if err != nil {
		return "", fmt.Errorf("%w for provider %q: invalid stored endpoint metadata: %v", ErrEndpointBindingMismatch, binding.Provider, err)
	}
	if stored != binding {
		return "", fmt.Errorf("%w for provider %q; run `metis login %s` again", ErrEndpointBindingMismatch, binding.Provider, binding.Provider)
	}
	return entry.Key, nil
}

// StoredAPIKeyEndpoint reports only endpoint metadata for a managed API key;
// it never returns the secret itself. A nil binding with present=true denotes
// a legacy unbound entry that must be re-authenticated before a custom
// endpoint can safely retain it.
func StoredAPIKeyEndpoint(provider string) (binding *EndpointBinding, present bool, err error) {
	provider, err = validateOAuthProviderID(provider)
	if err != nil {
		return nil, false, err
	}
	f, err := Load()
	if err != nil {
		return nil, false, err
	}
	var entry Entry
	found := false
	for storedProvider, candidate := range f {
		if canonicalOAuthProviderID(storedProvider) == provider {
			entry, found = candidate, true
			break
		}
	}
	if !found {
		return nil, false, nil
	}
	if isNamespacedKey(provider) || entry.Type != "api" {
		return nil, false, errors.New("auth: stored credential is not an LLM API key")
	}
	if entry.Endpoint == nil {
		return nil, true, nil
	}
	copy := *entry.Endpoint
	return &copy, true, nil
}

// Remove deletes a provider entry. No-op if it didn't exist.
func Remove(provider string) error {
	return updateAuthStore(func(f File) { delete(f, provider) })
}

func updateAuthStore(update func(File)) error {
	layout, err := currentCredentialLayout()
	if err != nil {
		return err
	}
	return withOAuthStoreLock(layout.auth, oauthStoreLockTimeout, func() error {
		file, err := loadAuthStoreUnlocked(layout)
		if err != nil {
			return err
		}
		update(file)
		if err := writeAuthStoreUnlocked(layout.auth, file); err != nil {
			return err
		}
		return removeLegacyCredentialFile(layout.legacyAuth)
	})
}

// List returns provider ids that have credentials, sorted. Excludes
// any namespaced keys (currently the "search:*" search-backend
// entries) — those are listed via ListSearchKeys.
func List() ([]string, error) {
	f, err := Load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f))
	for k := range f {
		if isNamespacedKey(k) {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// searchKeyPrefix is the namespace inside auth.json for WebSearch
// backend API keys. Stored under the same flat map as provider
// credentials (avoids a schema migration) but kept logically
// separate via the prefix so Get("tavily") can never accidentally
// pick up a Tavily search key when an LLM provider called "tavily"
// is added in the future.
const searchKeyPrefix = "search:"

// isNamespacedKey reports whether a raw auth.json key belongs to a
// non-LLM-provider namespace (search keys etc.). Used by List() to
// hide search keys from the provider list and vice-versa.
func isNamespacedKey(k string) bool {
	return len(k) > len(searchKeyPrefix) && k[:len(searchKeyPrefix)] == searchKeyPrefix
}

// GetSearchKey returns the persisted API key for a WebSearch
// backend, or empty string if not set. Distinct from Get() so it
// can't accidentally collide with provider credentials (search:tavily
// vs. tavily-as-llm-provider). Empty backend → empty result, never
// errors on a missing entry; the caller decides whether to fall
// through to env vars or skip the backend.
func GetSearchKey(backend string) (string, error) {
	if backend == "" {
		return "", nil
	}
	f, err := Load()
	if err != nil {
		return "", err
	}
	if e, ok := f[searchKeyPrefix+backend]; ok {
		return e.Key, nil
	}
	return "", nil
}

// SetSearchKey persists a WebSearch backend API key. Validates both
// names so callers can't write `search:` with an empty backend or
// store an empty key.
func SetSearchKey(backend, key string) error {
	if backend == "" {
		return errors.New("auth: backend required")
	}
	if key == "" {
		return errors.New("auth: key required")
	}
	return updateAuthStore(func(f File) {
		f[searchKeyPrefix+backend] = Entry{Type: "search", Key: key}
	})
}

// RemoveSearchKey deletes a backend's stored key. No-op when absent.
func RemoveSearchKey(backend string) error {
	if backend == "" {
		return errors.New("auth: backend required")
	}
	return updateAuthStore(func(f File) { delete(f, searchKeyPrefix+backend) })
}

// ListSearchKeys returns the names of all WebSearch backends that
// have a persisted key, sorted. Names exclude the "search:" prefix.
func ListSearchKeys() ([]string, error) {
	f, err := Load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for k := range f {
		if !isNamespacedKey(k) {
			continue
		}
		out = append(out, k[len(searchKeyPrefix):])
	}
	sort.Strings(out)
	return out, nil
}
