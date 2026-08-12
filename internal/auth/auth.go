// Package auth manages provider credentials persisted to ~/.metis/auth.json.
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
// Keeping them split also lets `metis auth login` rewrite credentials atomically
// without touching unrelated config.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// home resolves the metis home directory the same way internal/config.Home() does.
// We deliberately don't import config here — config consults auth.Get() inside
// ResolveAPIKey, so importing back would form a cycle. Keeping a private copy is
// fine because the rule (env METIS_HOME wins, else ~/.metis) is short and stable.
func home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	hd, _ := os.UserHomeDir()
	return filepath.Join(hd, ".metis")
}

// File is the on-disk shape of ~/.metis/auth.json.
// Keyed by provider id ("anthropic", "openai", "minimax", or any custom id).
type File map[string]Entry

// Entry is one provider's credential. `Type` is currently always "api"; it's
// kept as a discriminator so future flows (oauth, instance creds) slot in
// without a schema migration.
type Entry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// Path returns the canonical location of auth.json under the metis home.
func Path() string {
	return filepath.Join(home(), "auth.json")
}

// Load reads auth.json. A missing file is NOT an error — it returns an empty
// map so callers can append-and-save without first checking existence.
func Load() (File, error) {
	p := Path()
	// Permission hygiene: auth.json holds API keys. minimax-cli's
	// `auth/credentials.ts` does the same check — if the file is
	// world- or group-readable, warn the user and tighten it back to
	// 0600 before reading. We don't refuse to load (that would brick
	// users mid-session over a permission glitch), just self-heal +
	// stderr warning. See `assertSecurePerms` below for the rule.
	assertSecurePerms(p)
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if len(b) == 0 {
		return File{}, nil
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if f == nil {
		f = File{}
	}
	return f, nil
}

// assertSecurePerms checks that the auth.json file at `p` is readable
// only by the owner. If group/other has any access bits set, we emit
// a one-line warning to stderr and chmod the file back to 0600. No-op
// when the file doesn't exist (Load handles that path itself).
//
// Skipped on Windows: NTFS perm bits don't translate to the Unix mask,
// and the equivalent check would have to inspect ACLs — separate
// codepath, file an issue if you actually run metis on Windows with
// auth.json.
var assertSecurePerms = assertSecurePermsUnix

func assertSecurePermsUnix(p string) {
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	mode := st.Mode().Perm()
	const ownerOnly = 0o600
	if mode&^ownerOnly == 0 {
		return // already tight enough
	}
	fmt.Fprintf(os.Stderr,
		"metis: %s has loose perms %#o; tightening to 0600\n", p, mode)
	if err := os.Chmod(p, ownerOnly); err != nil {
		fmt.Fprintf(os.Stderr, "metis: chmod %s: %v\n", p, err)
	}
}

// Save writes auth.json atomically with 0o600 perms.
// Atomic = write to a sibling temp file then rename, so a crash mid-write
// can't leave a half-truncated credentials file.
func Save(f File) error {
	dir := home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
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

	final := Path()
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
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, final, err)
	}
	return nil
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
	f, err := Load()
	if err != nil {
		return err
	}
	f[provider] = Entry{Type: "api", Key: key}
	return Save(f)
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

// Remove deletes a provider entry. No-op if it didn't exist.
func Remove(provider string) error {
	f, err := Load()
	if err != nil {
		return err
	}
	if _, ok := f[provider]; !ok {
		return nil
	}
	delete(f, provider)
	return Save(f)
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
	f, err := Load()
	if err != nil {
		return err
	}
	f[searchKeyPrefix+backend] = Entry{Type: "search", Key: key}
	return Save(f)
}

// RemoveSearchKey deletes a backend's stored key. No-op when absent.
func RemoveSearchKey(backend string) error {
	if backend == "" {
		return errors.New("auth: backend required")
	}
	f, err := Load()
	if err != nil {
		return err
	}
	full := searchKeyPrefix + backend
	if _, ok := f[full]; !ok {
		return nil
	}
	delete(f, full)
	return Save(f)
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
