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
