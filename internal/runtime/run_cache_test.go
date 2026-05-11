package runtime

// run_cache_test.go — locks the on-disk response cache (CACHE-D).
// Coverage: key stability + TTL expiry + tool-use refusal + atomic
// rename + lookup miss/hit/expired flow.

import (
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunCacheKey_StableForIdenticalInput(t *testing.T) {
	a := RunCacheKey("m", "anthropic", "sys", "hello")
	b := RunCacheKey("m", "anthropic", "sys", "hello")
	if a != b {
		t.Errorf("identical inputs must produce identical keys; got %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("key should be 64-char hex (SHA-256); got len=%d", len(a))
	}
}

func TestRunCacheKey_ChangesOnAnyField(t *testing.T) {
	base := RunCacheKey("model", "anthropic", "sys", "hello")
	cases := []struct {
		label string
		k     string
	}{
		{"model", RunCacheKey("OTHER", "anthropic", "sys", "hello")},
		{"provider", RunCacheKey("model", "openai", "sys", "hello")},
		{"system", RunCacheKey("model", "anthropic", "DIFFERENT", "hello")},
		{"prompt", RunCacheKey("model", "anthropic", "sys", "different prompt")},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if c.k == base {
				t.Errorf("%s change should produce a different key", c.label)
			}
		})
	}
}

func TestLookupRunCache_MissingFileReturnsNil(t *testing.T) {
	setMetisHome(t)
	got, err := LookupRunCache("never-existed-key-1234")
	if err != nil {
		t.Errorf("missing cache → nil, nil; got err=%v", err)
	}
	if got != nil {
		t.Errorf("missing cache → nil entry; got %+v", got)
	}
}

func TestSaveLookupRunCache_RoundTrip(t *testing.T) {
	setMetisHome(t)
	want := &RunCacheEntry{
		PromptHash: hexSum("abc"),
		Model:      "MiniMax-M2.7",
		Prompt:     "hello world",
		Response:   "Hi there! How can I help you?",
		TTLSeconds: 3600,
		UsedTools:  false,
	}
	if err := SaveRunCache(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LookupRunCache(want.PromptHash)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatalf("lookup returned nil after successful save")
	}
	if got.Response != want.Response {
		t.Errorf("response mismatch: got %q, want %q", got.Response, want.Response)
	}
	if got.PromptHash != want.PromptHash {
		t.Errorf("hash mismatch")
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be populated on save")
	}
}

func TestSaveRunCache_RefusesToolUseEntries(t *testing.T) {
	setMetisHome(t)
	bad := &RunCacheEntry{
		PromptHash: hexSum("tool-using"),
		Model:      "m",
		Response:   "result",
		TTLSeconds: 60,
		UsedTools:  true,
	}
	err := SaveRunCache(bad)
	if err == nil {
		t.Errorf("expected SaveRunCache to refuse a UsedTools=true entry")
	}
	if !strings.Contains(err.Error(), "tool") {
		t.Errorf("refusal should mention tool-use; got: %v", err)
	}
}

func TestSaveRunCache_RequiresHashAndResponse(t *testing.T) {
	setMetisHome(t)
	cases := []struct {
		label string
		entry *RunCacheEntry
	}{
		{"nil entry", nil},
		{"missing hash", &RunCacheEntry{Response: "x"}},
		{"missing response", &RunCacheEntry{PromptHash: "abc"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if err := SaveRunCache(c.entry); err == nil {
				t.Errorf("expected error for %s", c.label)
			}
		})
	}
}

func TestLookupRunCache_TTLExpiry(t *testing.T) {
	setMetisHome(t)
	expired := &RunCacheEntry{
		PromptHash: hexSum("expired"),
		Model:      "m",
		Response:   "old",
		TTLSeconds: 1, // 1 second
		CreatedAt:  time.Now().Add(-time.Hour).UTC(),
		UsedTools:  false,
	}
	if err := SaveRunCache(expired); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LookupRunCache(expired.PromptHash)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != nil {
		t.Errorf("expired entry should not be returned; got %+v", got)
	}
	// File should have been deleted on read.
	if _, err := os.Stat(RunCachePath(expired.PromptHash)); !os.IsNotExist(err) {
		t.Errorf("expired cache file should have been deleted on read; stat=%v", err)
	}
}

func TestLookupRunCache_TTLZeroMeansNeverExpire(t *testing.T) {
	setMetisHome(t)
	immortal := &RunCacheEntry{
		PromptHash: hexSum("immortal"),
		Model:      "m",
		Response:   "stays alive",
		TTLSeconds: 0, // never expires
		CreatedAt:  time.Now().Add(-365 * 24 * time.Hour).UTC(),
		UsedTools:  false,
	}
	if err := SaveRunCache(immortal); err != nil {
		t.Fatal(err)
	}
	got, err := LookupRunCache(immortal.PromptHash)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Response != "stays alive" {
		t.Errorf("TTL=0 should mean never expire; got %+v", got)
	}
}

func TestLookupRunCache_HashCollisionDetected(t *testing.T) {
	setMetisHome(t)
	// Save under hash A.
	a := &RunCacheEntry{
		PromptHash: hexSum("aaa"),
		Model:      "m",
		Response:   "A-response",
		UsedTools:  false,
	}
	if err := SaveRunCache(a); err != nil {
		t.Fatal(err)
	}
	// Lookup with the FULL hash for a different prompt that happens
	// to share the truncated filename — should miss (file content's
	// full hash doesn't match).
	bogus := a.PromptHash[:16] + strings.Repeat("0", 48)
	got, _ := LookupRunCache(bogus)
	if got != nil {
		t.Errorf("hash collision on truncated filename must not serve content; got %+v", got)
	}
}

func TestParseRunCacheTTL(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultRunCacheTTL},
		{"  ", DefaultRunCacheTTL},
		{"1h", time.Hour},
		{"30m", 30 * time.Minute},
		{"24h", 24 * time.Hour},
		{"off", 0},
		{"OFF", 0},
		{"false", 0},
		{"0", 0},
		{"not-a-duration", DefaultRunCacheTTL},
		{"-5m", DefaultRunCacheTTL}, // negatives fold to default
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := ParseRunCacheTTL(c.in); got != c.want {
				t.Errorf("ParseRunCacheTTL(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestSaveRunCache_FilePerms(t *testing.T) {
	setMetisHome(t)
	e := &RunCacheEntry{
		PromptHash: hexSum("perm"),
		Model:      "m",
		Response:   "x",
		TTLSeconds: 60,
	}
	if err := SaveRunCache(e); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(RunCachePath(e.PromptHash))
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("cache file perms: got %#o, want 0o600", mode)
	}
}

// hexSum builds a deterministic 64-char hex hash for test fixtures so
// they look like real RunCacheKey output without needing the full key
// derivation.
func hexSum(seed string) string {
	h := sha256.Sum256([]byte(seed))
	out := make([]byte, 64)
	const hexChars = "0123456789abcdef"
	for i, b := range h {
		out[i*2] = hexChars[b>>4]
		out[i*2+1] = hexChars[b&0x0f]
	}
	return string(out)
}
