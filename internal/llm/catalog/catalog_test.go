package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixture is a tiny 2-provider catalog payload. We don't ship the full
// 117-provider models.dev dump in tests — only the contract the parser
// + lookups exercise.
const fixture = `{
  "minimax": {
    "name": "MiniMax",
    "api": "https://api.minimaxi.com/anthropic",
    "npm": "@ai-sdk/anthropic",
    "env": ["MINIMAX_API_KEY"],
    "models": {
      "MiniMax-M2.7": {
        "name": "M2.7",
        "tool_call": true,
        "limit": {"context": 192000, "output": 64000},
        "cost": {"input": 0.2, "output": 0.6}
      }
    }
  },
  "deepseek": {
    "name": "DeepSeek",
    "api": "https://api.deepseek.com",
    "npm": "@ai-sdk/openai-compatible",
    "env": ["DEEPSEEK_API_KEY"],
    "models": {
      "deepseek-chat": {"tool_call": true, "limit": {"context": 65536, "output": 8192}}
    }
  }
}`

func newServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func newClientFor(t *testing.T, srvURL string) *Client {
	t.Helper()
	dir := t.TempDir()
	c := NewClient(dir)
	c.URL = srvURL
	c.TTL = 100 * time.Millisecond // short for tests
	return c
}

// TestGet_NetworkFreshFetch: empty disk + working network → catalog
// loaded + persisted to disk.
func TestGet_NetworkFreshFetch(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)

	cat, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := cat["minimax"]; !ok {
		t.Errorf("minimax missing from cat: %+v", cat)
	}
	if _, ok := cat["deepseek"]; !ok {
		t.Errorf("deepseek missing from cat")
	}
	if _, err := os.Stat(c.CachePath); err != nil {
		t.Errorf("cache file not persisted: %v", err)
	}
}

// TestGet_FallbackToDiskOnNetworkFail: cache exists, network is down →
// returns cached copy (no error).
func TestGet_FallbackToDiskOnNetworkFail(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache", "models.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte(fixture), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	c := NewClient(dir)
	c.URL = "http://127.0.0.1:1" // unreachable
	c.HTTP.Timeout = 200 * time.Millisecond

	cat, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("Get with cached fallback: %v", err)
	}
	if _, ok := cat["minimax"]; !ok {
		t.Error("expected minimax loaded from cache")
	}
}

// TestGet_NoCacheNoNetwork: empty disk + dead network → error.
func TestGet_NoCacheNoNetwork(t *testing.T) {
	c := NewClient(t.TempDir())
	c.URL = "http://127.0.0.1:1"
	c.HTTP.Timeout = 100 * time.Millisecond

	_, err := c.Get(context.Background())
	if err == nil {
		t.Error("expected error with no cache and no network")
	}
}

// TestProvider_Lookup: id-keyed lookup returns ok=true with id back-
// filled from the map key.
func TestProvider_Lookup(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)

	p, ok, err := c.Provider(context.Background(), "minimax")
	if err != nil || !ok {
		t.Fatalf("Provider(minimax): ok=%v err=%v", ok, err)
	}
	if p.ID != "minimax" {
		t.Errorf("ID backfill: got %q, want minimax", p.ID)
	}
	if p.NPM != "@ai-sdk/anthropic" {
		t.Errorf("npm: got %q", p.NPM)
	}
}

func TestProvider_Unknown(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)

	_, ok, err := c.Provider(context.Background(), "no-such-provider")
	if err != nil {
		t.Errorf("err on unknown: %v", err)
	}
	if ok {
		t.Error("expected ok=false on unknown provider")
	}
}

// TestModel_Lookup: provider+model id → Model with id backfilled.
func TestModel_Lookup(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)

	m, ok, err := c.Model(context.Background(), "minimax", "MiniMax-M2.7")
	if err != nil || !ok {
		t.Fatalf("Model: ok=%v err=%v", ok, err)
	}
	if m.Limit.Context != 192000 {
		t.Errorf("context: got %d, want 192000", m.Limit.Context)
	}
	if !m.ToolCall {
		t.Error("tool_call: expected true")
	}
}

func TestLookupVisionByModelIDAggregatesDuplicateRoutesDeterministically(t *testing.T) {
	const duplicateFixture = `{
	  "text-route": {"models": {"shared-model": {"modalities": {"input": ["text"]}}}},
	  "vision-route": {"models": {"shared-model": {"modalities": {"input": ["text", "image"]}}}}
}`
	srv := newServer(t, duplicateFixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("warm Get: %v", err)
	}
	for i := 0; i < 100; i++ {
		if supported, found := c.LookupVisionByModelID("shared-model"); !found || !supported {
			t.Fatalf("lookup %d = supported=%v found=%v, want any supporting route to win", i, supported, found)
		}
	}
	if supported, found := c.LookupVisionByModelID("missing"); found || supported {
		t.Fatalf("missing lookup = supported=%v found=%v", supported, found)
	}
}

// TestRefresh_ForcesNetwork: even within TTL, Refresh re-fetches.
func TestRefresh_ForcesNetwork(t *testing.T) {
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	c.TTL = 1 * time.Hour // long enough that Get wouldn't normally re-fetch

	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("Get without Refresh hit network %d times, want 1", count)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if count != 2 {
		t.Errorf("Refresh should add a hit; total %d, want 2", count)
	}
}

// TestTransportHint covers each known mapping plus the unsupported
// fallback. New transports added to BuildProvider should grow this
// table — keep it in sync.
func TestTransportHint(t *testing.T) {
	cases := map[string]string{
		"@ai-sdk/anthropic":               "anthropic_messages",
		"@ai-sdk/openai":                  "openai_chat",
		"@ai-sdk/openai-compatible":       "openai_chat",
		"@ai-sdk/google":                  "gemini_native",
		"@ai-sdk/azure":                   "azure_openai",
		"@ai-sdk/google-vertex":           "vertex_anthropic",
		"@ai-sdk/google-vertex/anthropic": "vertex_anthropic",
		"@ai-sdk/amazon-bedrock":          "bedrock_anthropic",
		"@openrouter/ai-sdk-provider":     "unsupported",
		"":                                "unsupported",
	}
	for npm, want := range cases {
		got := TransportHint(npm)
		if got != want {
			t.Errorf("TransportHint(%q) = %q, want %q", npm, got, want)
		}
	}
}

// TestPersist_AtomicRename: writes go to .tmp first then rename, so a
// crash mid-write doesn't leave a half-written file.
func TestPersist_AtomicRename(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Manually verify the cache file is valid JSON (regression
	// catch for "wrote tmp but failed rename" scenarios).
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var v Catalog
	if err := json.Unmarshal(data, &v); err != nil {
		t.Errorf("cache file isn't valid JSON: %v", err)
	}
	if _, ok := v["minimax"]; !ok {
		t.Error("cache content lost minimax key")
	}
}

// TestLookupContextWindowByModelID_HitAfterFetch — after a successful
// Get(), the synchronous lookup answers with the published window for
// any model in the fixture. This is the path provider.MaxContextTokens
// calls from its hot path without taking a network round-trip.
func TestLookupContextWindowByModelID_HitAfterFetch(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)

	// Warm the cache.
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("warm Get: %v", err)
	}

	// MiniMax model id → its published 192000 window.
	if got, ok := c.LookupContextWindowByModelID("MiniMax-M2.7"); !ok || got != 192000 {
		t.Errorf("MiniMax-M2.7: got (%d, %v), want (192000, true)", got, ok)
	}
	// DeepSeek model id → 65536 (the fixture's DeepSeek chat entry).
	if got, ok := c.LookupContextWindowByModelID("deepseek-chat"); !ok || got != 65536 {
		t.Errorf("deepseek-chat: got (%d, %v), want (65536, true)", got, ok)
	}
}

// TestLookupContextWindowByModelID_DuplicateRoutesAreAmbiguous covers models
// that are re-published by several gateways with different advertised limits.
// A model-only lookup must not silently pick one provider's value.
func TestLookupContextWindowByModelID_DuplicateRoutesAreAmbiguous(t *testing.T) {
	c := newClientFor(t, "http://invalid")
	c.cached = Catalog{
		"official": Provider{Models: map[string]Model{
			"glm-5.3": {Limit: Limit{Context: 1_000_000}},
		}},
		"gateway-a": Provider{Models: map[string]Model{
			"glm-5.3": {Limit: Limit{Context: 1_048_576}},
		}},
		"gateway-b": Provider{Models: map[string]Model{
			"glm-5.3": {Limit: Limit{Context: 1_048_560}},
		}},
	}

	if got, ok := c.LookupContextWindowByModelID("glm-5.3"); ok || got != 0 {
		t.Fatalf("ambiguous model-only lookup: got (%d, %v), want (0, false)", got, ok)
	}
}

func TestLookupContextWindow_UsesProviderAndModel(t *testing.T) {
	c := newClientFor(t, "http://invalid")
	c.cached = Catalog{
		"zhipuai": Provider{Models: map[string]Model{
			"glm-5.3": {Limit: Limit{Context: 1_000_000}},
		}},
		"gateway": Provider{Models: map[string]Model{
			"glm-5.3": {Limit: Limit{Context: 1_048_576}},
		}},
	}

	tests := []struct {
		providerID string
		modelID    string
		want       int
		wantOK     bool
	}{
		{providerID: "zhipuai", modelID: "glm-5.3", want: 1_000_000, wantOK: true},
		{providerID: "gateway", modelID: "glm-5.3", want: 1_048_576, wantOK: true},
		{providerID: "missing", modelID: "glm-5.3"},
		{providerID: "zhipuai", modelID: "missing"},
	}
	for _, tt := range tests {
		got, ok := c.LookupContextWindow(tt.providerID, tt.modelID)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("LookupContextWindow(%q, %q) = (%d, %v), want (%d, %v)",
				tt.providerID, tt.modelID, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestLookupContextWindowByModelID_MissBeforeFetch — without a prior
// Get(), the in-memory cache is nil and lookup must return ok=false
// rather than panic or block. This is the cold-start path where the
// background warm-up hasn't completed yet — provider falls back to
// its hardcoded prefix table, which is the documented contract.
func TestLookupContextWindowByModelID_MissBeforeFetch(t *testing.T) {
	c := newClientFor(t, "http://invalid")
	if got, ok := c.LookupContextWindowByModelID("anything"); ok || got != 0 {
		t.Errorf("cold cache: got (%d, %v), want (0, false)", got, ok)
	}
}

// TestLookupContextWindowByModelID_MissOnUnknownModel — populated
// cache, but the model id isn't in any provider. Must still cleanly
// return ok=false so the provider falls through to prefix / *k
// suffix parsing.
func TestLookupContextWindowByModelID_MissOnUnknownModel(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	_, _ = c.Get(context.Background())
	if got, ok := c.LookupContextWindowByModelID("never-published-anywhere"); ok || got != 0 {
		t.Errorf("unknown model: got (%d, %v), want (0, false)", got, ok)
	}
}

// TestLookupContextWindowByModelID_EmptyIDIsNotABlankWildcard —
// passing "" must NOT match the first provider's first model (would
// cause silent wrong-context-window when a misconfigured provider
// reports an empty Model name on streaming responses).
func TestLookupContextWindowByModelID_EmptyIDIsNotABlankWildcard(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	_, _ = c.Get(context.Background())
	if got, ok := c.LookupContextWindowByModelID(""); ok {
		t.Errorf("empty id should miss; got (%d, %v)", got, ok)
	}
}

// TestStat_AfterFetch — Stat() reports populated in-memory state
// plus on-disk cache stat. Pins the contract MetisInfo + `metis
// models status` rely on for surface tier-of-truth.
func TestStat_AfterFetch(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("warm Get: %v", err)
	}

	st := c.Stat()
	if !st.InMemory {
		t.Errorf("Stat.InMemory should be true after Get; got false")
	}
	if st.ProviderCount != 2 {
		t.Errorf("Stat.ProviderCount = %d; want 2 (fixture has minimax + deepseek)", st.ProviderCount)
	}
	if st.ModelCount != 2 {
		t.Errorf("Stat.ModelCount = %d; want 2 (1 minimax + 1 deepseek)", st.ModelCount)
	}
	if st.CacheBytes == 0 {
		t.Errorf("Stat.CacheBytes should reflect persisted cache size; got 0")
	}
	if st.CacheModTime.IsZero() {
		t.Errorf("Stat.CacheModTime should be set after persist; got zero")
	}
}

// TestStat_BeforeFetch — never-loaded client reports empty Stat
// rather than panicking. This is the cold-start path MetisInfo
// hits when the background warm-up hasn't completed yet.
func TestStat_BeforeFetch(t *testing.T) {
	c := newClientFor(t, "http://invalid")
	st := c.Stat()
	if st.InMemory {
		t.Errorf("cold client: InMemory should be false; got true")
	}
	if st.ProviderCount != 0 || st.ModelCount != 0 {
		t.Errorf("cold client counts should be zero; got providers=%d models=%d", st.ProviderCount, st.ModelCount)
	}
}

// TestLookupModel_MultiProviderHit — when the same model id is
// published under multiple providers (mirror / re-publish path),
// LookupModel must return all of them. Single-hit case is covered
// by the existing LookupContextWindowByModelID test family.
func TestLookupModel_AllHits(t *testing.T) {
	srv := newServer(t, fixture)
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	_, _ = c.Get(context.Background())

	hits := c.LookupModel("MiniMax-M2.7")
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for MiniMax-M2.7; got %d", len(hits))
	}
	if hits[0].ProviderID != "minimax" {
		t.Errorf("hit provider = %q; want minimax", hits[0].ProviderID)
	}
	if hits[0].Model.Limit.Context != 192000 {
		t.Errorf("hit context = %d; want 192000", hits[0].Model.Limit.Context)
	}

	if got := c.LookupModel("never-published"); got != nil {
		t.Errorf("unknown id should return nil; got %+v", got)
	}
	if got := c.LookupModel(""); got != nil {
		t.Errorf("empty id should return nil; got %+v", got)
	}
}
