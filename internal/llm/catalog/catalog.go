// Package catalog fetches LLM provider + model metadata from
// https://models.dev (the public catalog maintained by sst.dev — same
// catalog opencode and crush use). Cache lives at
// `~/.metis/cache/models.json` with a 5-minute TTL; stale reads are
// served if the network is down.
//
// Why a public catalog: hand-rolling per-provider context windows /
// pricing / capability matrices for 100+ models is unmaintainable.
// models.dev publishes 117 providers with full metadata — context
// limits, cost tables, modalities, tool-call support, recommended
// transport (anthropic / openai-compatible / azure / bedrock / etc.) —
// keyed by provider id. We just consume it.
//
// This package has zero metis-internal dependencies (no config / no
// llm imports) so it can be used from cmd/metis/cmd_models.go without
// pulling the whole runtime tree.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultURL is models.dev's public api.json. Override via the
// METIS_MODELS_URL env var for self-hosted mirrors.
const DefaultURL = "https://models.dev/api.json"

// DefaultTTL is how long we trust an on-disk snapshot before re-
// fetching. 5 minutes mirrors opencode's TTL — long enough to skip
// network on tab-tab-tab `metis models` calls, short enough that newly
// published models surface within a chat session.
const DefaultTTL = 5 * time.Minute

// Provider describes one entry in the catalog. Field names mirror
// models.dev's wire shape so users can search the upstream JSON and
// find the same keys.
type Provider struct {
	ID     string           `json:"id"`     // canonical provider key (lowercase, e.g. "minimax", "amazon-bedrock")
	Name   string           `json:"name"`   // human-readable
	API    string           `json:"api"`    // default API endpoint (often empty for cloud-auth providers like bedrock)
	NPM    string           `json:"npm"`    // recommended transport: @ai-sdk/anthropic | @ai-sdk/openai-compatible | @ai-sdk/amazon-bedrock | …
	Env    []string         `json:"env"`    // required env vars (auth)
	Doc    string           `json:"doc"`    // doc URL (optional)
	Models map[string]Model `json:"models"` // keyed by model id
}

// Model is a single offered model. Fields cover the bits metis cares
// about: context window for compaction, tool_call support for tool-use
// gating, cost for the eventual /cost slash command.
type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	ReleaseDate string `json:"release_date"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	Attachment  bool   `json:"attachment"`
	Cost        Cost   `json:"cost"`
	Limit       Limit  `json:"limit"`
	Status      string `json:"status"` // "" | alpha | beta | deprecated
	Temperature bool   `json:"temperature"`
}

// Cost is the per-million-token pricing in USD. Optional because some
// models (e.g. Bedrock variants billed per-region) omit it.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Limit is the model's context shape. Context is the total window
// (input + output combined); Output is the per-response cap.
type Limit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

// Catalog is the in-memory representation of the full models.dev
// dataset. Keyed by provider id.
type Catalog map[string]Provider

// Client fetches and caches the catalog. Safe for concurrent use; one
// in-flight fetch is shared via singleflight semantics (sync.Once per
// fetch attempt).
type Client struct {
	URL       string
	CachePath string
	TTL       time.Duration
	HTTP      *http.Client

	mu      sync.RWMutex
	cached  Catalog
	loadedT time.Time
}

// NewClient builds a Client with sensible defaults. Override fields
// directly for tests.
func NewClient(home string) *Client {
	url := os.Getenv("METIS_MODELS_URL")
	if url == "" {
		url = DefaultURL
	}
	return &Client{
		URL:       url,
		CachePath: filepath.Join(home, "cache", "models.json"),
		TTL:       DefaultTTL,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Get returns the catalog, fetching from network if the cache is stale
// or missing. Network failures fall back to whatever's on disk; only
// a completely empty cache + dead network surfaces as an error.
func (c *Client) Get(ctx context.Context) (Catalog, error) {
	c.mu.RLock()
	if c.cached != nil && time.Since(c.loadedT) < c.TTL {
		out := c.cached
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check after acquiring write lock — another goroutine may have
	// loaded while we were waiting.
	if c.cached != nil && time.Since(c.loadedT) < c.TTL {
		return c.cached, nil
	}

	// Try network first when cache is stale; fall back to whatever's on
	// disk if network fails. Order matters: a network success refreshes
	// the on-disk snapshot too, so subsequent offline starts are fast.
	if cat, err := c.fetchAndPersist(ctx); err == nil {
		c.cached = cat
		c.loadedT = time.Now()
		return cat, nil
	}

	if cat, err := c.loadFromDisk(); err == nil {
		c.cached = cat
		c.loadedT = time.Now()
		return cat, nil
	} else if c.cached != nil {
		// Stale in-memory copy is better than nothing.
		return c.cached, nil
	} else {
		return nil, fmt.Errorf("catalog: network unreachable and no cached copy at %s", c.CachePath)
	}
}

// Provider looks up one provider by id. Returns ok=false if the id
// isn't in the catalog.
func (c *Client) Provider(ctx context.Context, id string) (Provider, bool, error) {
	cat, err := c.Get(ctx)
	if err != nil {
		return Provider{}, false, err
	}
	p, ok := cat[id]
	if ok {
		p.ID = id // catalog entries don't always set ID — backfill from the map key
	}
	return p, ok, nil
}

// Model looks up one model under a provider. Returns ok=false if
// either the provider or the model id is unknown.
func (c *Client) Model(ctx context.Context, providerID, modelID string) (Model, bool, error) {
	p, ok, err := c.Provider(ctx, providerID)
	if err != nil || !ok {
		return Model{}, false, err
	}
	m, ok := p.Models[modelID]
	if ok {
		m.ID = modelID
	}
	return m, ok, nil
}

// Refresh forces a network fetch regardless of TTL. Useful for `metis
// models --refresh` and post-install warm-up.
func (c *Client) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cat, err := c.fetchAndPersist(ctx)
	if err != nil {
		return err
	}
	c.cached = cat
	c.loadedT = time.Now()
	return nil
}

func (c *Client) fetchAndPersist(ctx context.Context) (Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("catalog: %s returned %d", c.URL, resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	cat := Catalog{}
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("catalog: decode %s: %w", c.URL, err)
	}
	// Persist to disk best-effort; failures don't block returning the
	// fresh copy. The next process start re-fetches if disk write
	// failed (cache TTL window is short anyway).
	_ = c.persist(cat)
	return cat, nil
}

func (c *Client) persist(cat Catalog) error {
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	tmp := c.CachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.CachePath)
}

func (c *Client) loadFromDisk() (Catalog, error) {
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		return nil, err
	}
	cat := Catalog{}
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, fmt.Errorf("catalog: decode cached %s: %w", c.CachePath, err)
	}
	return cat, nil
}

// LookupContextWindowByModelID scans the in-memory catalog for any
// provider that publishes a model with the given id and returns its
// Limit.Context. ok=false when the cache hasn't been populated yet
// (nobody called Get) or no provider lists this model.
//
// Synchronous + read-only — safe to call from provider.MaxContextTokens
// hot paths. Never makes a network request; callers that need
// fresh data must call Get explicitly before this.
//
// Model IDs in models.dev are globally unique enough that scanning
// without a provider hint is fine (e.g. "deepseek-v4-pro" only
// appears under the deepseek provider). The first hit wins; the only
// realistic collision shape is a self-hosted re-publish of a hosted
// model under a different provider name, which would carry the same
// window anyway.
func (c *Client) LookupContextWindowByModelID(modelID string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return 0, false
	}
	for _, p := range c.cached {
		if m, ok := p.Models[modelID]; ok && m.Limit.Context > 0 {
			return m.Limit.Context, true
		}
	}
	return 0, false
}

// defaultClient + defaultOnce wire a package-level singleton so the
// provider.MaxContextTokens fast path can call catalog without each
// provider package threading a *Client through its constructor. The
// background warm-up fires on first Default() call; if it succeeds
// LookupContextWindowByModelID starts answering with real numbers
// within a few hundred ms of process start. If it fails (offline,
// DNS hiccup), the lookup stays empty and providers fall through to
// their hardcoded prefix tables — that's why hardcoded stays as the
// belt to catalog's braces.
var (
	defaultClient *Client
	defaultOnce   sync.Once
)

// Default returns the process-wide catalog client, kicking off a
// background fetch on first call. Reuses the standard cache location
// (~/.metis/cache/models.json). Returns nil if HOME is unset (CI
// path) or METIS_CATALOG_DISABLE=1 is set in env (test path).
// Callers must nil-check.
func Default() *Client {
	defaultOnce.Do(func() {
		// Test escape hatch — provider unit tests want to exercise
		// the prefix / suffix tiers in MaxContextTokens without the
		// background fetch racing in and overriding their fixtures.
		// Setting METIS_CATALOG_DISABLE=1 keeps the singleton nil so
		// the provider's nil-check falls through to the next tier.
		if os.Getenv("METIS_CATALOG_DISABLE") == "1" {
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		defaultClient = NewClient(filepath.Join(home, ".metis"))
		// Background warm-up. 8-second context cap so a hanging
		// upstream doesn't leak a goroutine for hours; the eventual
		// fetch failure is silent — callers see ok=false and fall
		// through to the prefix table, which is fine.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_, _ = defaultClient.Get(ctx)
		}()
	})
	return defaultClient
}

// TransportHint maps the catalog's `npm` field to one of metis's
// internal transport names. Every transport name here has a
// corresponding case in runtime/provider.go's BuildProvider — keep
// the two in sync.
func TransportHint(npm string) string {
	switch npm {
	case "@ai-sdk/anthropic":
		return "anthropic_messages"
	case "@ai-sdk/openai", "@ai-sdk/openai-compatible":
		return "openai_chat"
	case "@ai-sdk/google":
		return "gemini_native"
	case "@ai-sdk/azure":
		return "azure_openai"
	case "@ai-sdk/google-vertex", "@ai-sdk/google-vertex/anthropic":
		return "vertex_anthropic"
	case "@ai-sdk/amazon-bedrock":
		return "bedrock_anthropic"
	default:
		return "unsupported"
	}
}
