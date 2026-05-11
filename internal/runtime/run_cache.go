package runtime

// run_cache.go — on-disk response cache for `metis run`.
//
// Use case: CI pipelines, cron jobs, scripted-replay scenarios where
// the user invokes `metis run <prompt>` repeatedly with identical
// inputs. The API call costs the same dollars every time even though
// the answer would be byte-identical; this cache short-circuits the
// round-trip entirely.
//
// Scope: ONLY `metis run` (one-shot). NOT enabled for `metis chat`
// (conversational state would poison the cache key).
//
// Safety: cache hits ONLY fire for turns that used no tools. Tool-use
// turns observe the world (file contents, git state, web pages) — a
// cached replay would lie about that observation. The save side
// detects tool usage and skips writing to the cache.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// RunCacheEntry is the on-disk schema. Stored as JSON for human
// inspection — users may want to grep / cat / rm individual entries
// without writing tooling.
type RunCacheEntry struct {
	PromptHash     string    `json:"prompt_hash"`
	Model          string    `json:"model"`
	Prompt         string    `json:"prompt"` // echoed for ops debugging
	Response       string    `json:"response"`
	CreatedAt      time.Time `json:"created_at"`
	TTLSeconds     int       `json:"ttl_seconds"`
	UsedTools      bool      `json:"used_tools"` // always false in saved entries — defensive
	InputTokens    int       `json:"input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	CacheReadTok   int       `json:"cache_read_input_tokens"`
	CacheCreateTok int       `json:"cache_creation_input_tokens"`
}

// RunCacheKey builds the canonical hash key used to look up cached
// responses. The hash covers everything that could change the
// answer:
//
//	model        — different models produce different responses
//	provider     — same model name across providers may behave differently
//	system       — system prompt edits change the answer surface
//	prompt       — the user's actual ask
//
// Deliberately EXCLUDES timestamps, session IDs, working directory,
// and any volatile env data — those don't affect the model's
// reasoning and including them would flap the cache on every
// invocation.
func RunCacheKey(model, provider, system, prompt string) string {
	h := sha256.New()
	fmt.Fprintf(h, "model:%s\n", model)
	fmt.Fprintf(h, "provider:%s\n", provider)
	fmt.Fprintf(h, "system-sha:%x\n", sha256.Sum256([]byte(system)))
	fmt.Fprintf(h, "prompt:%s\n", prompt)
	return hex.EncodeToString(h.Sum(nil))
}

// RunCacheDir returns the cache root under metis home.
func RunCacheDir() string {
	return filepath.Join(config.Home(), "run-cache")
}

// RunCachePath returns the disk path for one key. We truncate the
// 64-char SHA-256 to its first 16 chars for shorter filenames —
// collision risk is 1 in 2^64, fine for an inspection-friendly cache.
func RunCachePath(key string) string {
	short := key
	if len(short) > 16 {
		short = short[:16]
	}
	return filepath.Join(RunCacheDir(), short+".json")
}

// LookupRunCache returns a cached entry if one exists, has not
// expired, and matches the full hash (not just the truncated
// filename). Returns (nil, nil) on miss; a non-nil error indicates a
// corrupted cache file rather than the lookup pattern being wrong.
//
// Auto-deletes expired entries on read so the cache directory
// doesn't grow unbounded with stale files.
func LookupRunCache(key string) (*RunCacheEntry, error) {
	p := RunCachePath(key)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var e RunCacheEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	// Full-hash check (file name is truncated → collision-resistant
	// but worth verifying with the full hash before serving content).
	if e.PromptHash != key {
		// Hash collision on truncated filename. Treat as miss.
		return nil, nil
	}
	// TTL check.
	if e.TTLSeconds > 0 {
		expiry := e.CreatedAt.Add(time.Duration(e.TTLSeconds) * time.Second)
		if time.Now().After(expiry) {
			_ = os.Remove(p)
			return nil, nil
		}
	}
	return &e, nil
}

// SaveRunCache writes an entry. Atomic via tempfile + rename so a
// crash mid-write doesn't strand future lookups with a half-parsed
// file. 0o600 because cached responses can contain whatever the LLM
// said — including potentially sensitive code or paths the user
// asked it to surface.
//
// Refuses to write entries marked UsedTools=true. The save site
// should already detect this and pass UsedTools accordingly; this
// belt-and-suspenders ensures a buggy caller can't poison the cache.
func SaveRunCache(e *RunCacheEntry) error {
	if e == nil {
		return fmt.Errorf("run_cache: nil entry")
	}
	if e.UsedTools {
		return fmt.Errorf("run_cache: refusing to cache tool-use turn (would lie about world state on replay)")
	}
	if e.PromptHash == "" || e.Response == "" {
		return fmt.Errorf("run_cache: hash and response required")
	}
	dir := RunCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	final := RunCachePath(e.PromptHash)
	tmp, err := os.CreateTemp(dir, ".run-cache.*.json")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// DefaultRunCacheTTL — 1 hour. Long enough to make a CI pipeline that
// runs `metis run` in 3 successive steps benefit; short enough that
// an upstream code change or doc edit won't be silently served the
// old answer for days.
const DefaultRunCacheTTL = time.Hour

// ParseRunCacheTTL converts a duration string (1h, 30m, 24h) into a
// time.Duration with the standard `time.ParseDuration` rules. Empty
// → default; "off"/"0" → 0 (disable). Returns the default on parse
// errors rather than erroring out — startup shouldn't fail because
// of a typoed cache flag.
func ParseRunCacheTTL(s string) time.Duration {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return DefaultRunCacheTTL
	}
	if v == "off" || v == "false" || v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return DefaultRunCacheTTL
	}
	return d
}
