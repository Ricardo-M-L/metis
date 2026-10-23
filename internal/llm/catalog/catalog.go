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
	"io"
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
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Family      string     `json:"family"`
	ReleaseDate string     `json:"release_date"`
	Reasoning   bool       `json:"reasoning"`
	ToolCall    bool       `json:"tool_call"`
	Attachment  bool       `json:"attachment"`
	Cost        Cost       `json:"cost"`
	Limit       Limit      `json:"limit"`
	Status      string     `json:"status"` // "" | alpha | beta | deprecated
	Temperature bool       `json:"temperature"`
	Modalities  Modalities `json:"modalities"`
}

// Modalities declares the input/output shapes a model accepts.
// models.dev's schema: input is one or more of
// "text"|"image"|"audio"|"video"|"pdf"; output is usually just "text".
// SupportsVision checks Input contains "image".
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// SupportsImage reports whether m accepts image input. Used by
// llm.OpenAI.SupportsVision / llm.Anthropic.SupportsVision as the
// catalog-backed replacement for the hardcoded prefix whitelist.
func (m Model) SupportsImage() bool {
	for _, mod := range m.Modalities.Input {
		if mod == "image" {
			return true
		}
	}
	return false
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

// Client fetches and caches the catalog. Safe for concurrent use; concurrent
// refresh callers share an in-flight request and capability lookups read the
// previous snapshot while that request is running.
type Client struct {
	URL       string
	CachePath string
	TTL       time.Duration
	HTTP      *http.Client

	mu        sync.RWMutex
	cached    Catalog
	loadedT   time.Time
	snapshotT time.Time
	source    string
	fetching  *catalogFetch
}

type catalogFetch struct {
	done chan struct{}
	cat  Catalog
	err  error
}

// NewClient builds a Client and loads an existing disk snapshot before
// returning. Even an expired snapshot is useful to synchronous capability
// lookups during cold start; its original modification time controls TTL,
// so reading an old file does not make its metadata fresh again. No network
// request is made by the constructor. Override fields directly for tests.
func NewClient(home string) *Client {
	url := os.Getenv("METIS_MODELS_URL")
	if url == "" {
		url = DefaultURL
	}
	c := &Client{
		URL:       url,
		CachePath: filepath.Join(home, "cache", "models.json"),
		TTL:       DefaultTTL,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
	if cat, modified, err := c.loadDiskSnapshot(); err == nil {
		c.cached, c.snapshotT = cat, modified
		c.loadedT, c.source = time.Now(), "disk_cache"
	}
	return c
}

// ReadOnlySnapshot returns a detached copy of the already loaded local
// catalog. It never refreshes metadata or performs network I/O.
func (c *Client) ReadOnlySnapshot() Catalog {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil {
		return nil
	}
	data, err := json.Marshal(c.cached)
	if err != nil {
		return nil
	}
	var snapshot Catalog
	if json.Unmarshal(data, &snapshot) != nil {
		return nil
	}
	return snapshot
}

// Get returns the catalog, fetching from network if the cache is stale
// or missing. Network failures fall back to whatever's on disk; only
// a completely empty cache + dead network surfaces as an error.
func (c *Client) Get(ctx context.Context) (Catalog, error) {
	cat, err := c.fetch(ctx, false)
	if err == nil {
		return cat, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.mu.RLock()
	cat = c.cached
	c.mu.RUnlock()
	if cat != nil {
		return cat, nil
	}
	// CachePath can be supplied after construction, and another process may
	// have produced a disk snapshot while this process was offline.
	if cat, modified, diskErr := c.loadDiskSnapshot(); diskErr == nil {
		c.mu.Lock()
		if c.cached == nil {
			c.cached, c.snapshotT = cat, modified
			c.loadedT, c.source = time.Now(), "disk_cache"
		} else {
			cat = c.cached
		}
		c.mu.Unlock()
		return cat, nil
	}
	return nil, fmt.Errorf("catalog: network unreachable and no cached copy at %s: %w", c.CachePath, err)
}

func (c *Client) freshLocked() bool {
	return c.cached != nil && (c.source == "static" ||
		(!c.snapshotT.IsZero() && c.TTL > 0 && time.Since(c.snapshotT) < c.TTL))
}

// fetch shares one request among callers without holding the metadata lock
// during HTTP or disk I/O. The initiating caller's context owns that request;
// a waiting caller can cancel its own wait without cancelling the initiator.
func (c *Client) fetch(ctx context.Context, force bool) (Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if !force && c.freshLocked() {
		cat := c.cached
		c.mu.Unlock()
		return cat, nil
	}
	if active := c.fetching; active != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-active.done:
			return active.cat, active.err
		}
	}
	active := &catalogFetch{done: make(chan struct{})}
	c.fetching = active
	c.mu.Unlock()

	cat, err := c.fetchAndPersist(ctx)
	c.mu.Lock()
	if err == nil {
		c.cached = cat
		c.loadedT, c.snapshotT = time.Now(), time.Now()
		c.source = "network"
	}
	active.cat, active.err = cat, err
	c.fetching = nil
	close(active.done)
	c.mu.Unlock()
	return cat, err
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
	_, err := c.fetch(ctx, true)
	return err
}

func (c *Client) fetchAndPersist(ctx context.Context) (Catalog, error) {
	if c.HTTP == nil {
		return nil, fmt.Errorf("catalog: HTTP client is not configured")
	}
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
	if cat == nil {
		return nil, fmt.Errorf("catalog: empty snapshot from %s", c.URL)
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
	file, err := os.CreateTemp(filepath.Dir(c.CachePath), "models-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, c.CachePath)
}

func (c *Client) loadFromDisk() (Catalog, error) {
	cat, _, err := c.loadDiskSnapshot()
	return cat, err
}

func (c *Client) loadDiskSnapshot() (Catalog, time.Time, error) {
	file, err := os.Open(c.CachePath)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, time.Time{}, fmt.Errorf("catalog: cached snapshot is not a regular file")
	}
	cat := Catalog{}
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&cat); err != nil {
		return nil, time.Time{}, fmt.Errorf("catalog: decode cached %s: %w", c.CachePath, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, time.Time{}, fmt.Errorf("catalog: trailing data in cached snapshot at %s", c.CachePath)
	}
	if cat == nil {
		return nil, time.Time{}, fmt.Errorf("catalog: empty cached snapshot at %s", c.CachePath)
	}
	return cat, info.ModTime(), nil
}

// LookupVisionByRoute returns the models.dev fact for one exact
// catalog provider/model pair. Prefer this over
// LookupVisionByModelID whenever the caller knows which route it is
// actually talking to: the same wire id is re-published by many
// gateways with different modalities, so a provider-blind answer can
// describe a route the user is not on.
//
// Synchronous + read-only — never makes a network request; the caller
// is responsible for triggering Get() at startup so the cache is warm.
func (c *Client) LookupVisionByRoute(providerID, modelID string) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || providerID == "" || modelID == "" {
		return false, false
	}
	p, ok := c.cached[providerID]
	if !ok {
		return false, false
	}
	m, ok := p.Models[modelID]
	if !ok {
		return false, false
	}
	return m.SupportsImage(), true
}

// LookupVisionByModelID is the provider-agnostic compatibility
// fallback. It succeeds only when every catalog route that publishes
// the model agrees on image support. Conflicting routes are ambiguous
// and return ok=false so the caller can fall through to family
// heuristics (or stay tri-state Unknown) instead of guessing a
// provider or depending on Go map iteration order.
//
// The earlier "any route that declares image wins" rule was
// deterministic but unsound in the unsafe direction: for ids like
// deepseek-v4-flash (image-capable on a few routes, text-only on the
// large majority) it reported VisionSupported, so images were shipped
// to a text-only endpoint and burned the turn on a 400. Mirrors
// LookupContextWindowByModelID, which has always required unanimity.
//
// Synchronous + read-only. Never makes a network request.
func (c *Client) LookupVisionByModelID(modelID string) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return false, false
	}
	found := false
	supported := false
	for _, p := range c.cached {
		m, ok := p.Models[modelID]
		if !ok {
			continue
		}
		if !found {
			found, supported = true, m.SupportsImage()
			continue
		}
		if m.SupportsImage() != supported {
			return false, false
		}
	}
	return supported, found
}

// LookupReasoningByModelID reports whether catalog metadata marks a model as
// reasoning-capable. Like the vision lookup this is synchronous, read-only,
// and never initiates network I/O; callers fall back to conservative local
// model-family facts when the cache is cold or the id is private.
func (c *Client) LookupReasoningByModelID(modelID string) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return false, false
	}
	found := false
	for _, p := range c.cached {
		if m, ok := p.Models[modelID]; ok {
			found = true
			if m.Reasoning {
				return true, true
			}
		}
	}
	return false, found
}

// LookupContextWindow returns the context limit for one exact catalog
// provider/model pair. Provider IDs are the top-level keys from models.dev,
// not transport names such as "openai_chat".
func (c *Client) LookupContextWindow(providerID, modelID string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || providerID == "" || modelID == "" {
		return 0, false
	}
	p, ok := c.cached[providerID]
	if !ok {
		return 0, false
	}
	m, ok := p.Models[modelID]
	if !ok || m.Limit.Context <= 0 {
		return 0, false
	}
	return m.Limit.Context, true
}

// ContextMatch identifies both the matched limit and the resolution path.
// A catalog fact describes its published route; it cannot establish what an
// undocumented proxy actually permits. Endpoint metadata and explicit user
// configuration should take priority over these compatibility lookups.
type ContextMatch struct {
	Window     int
	Source     string // "catalog_route" or "catalog_model"
	ProviderID string // populated only for an exact route match
	Stale      bool   // snapshot age exceeds TTL, including offline fallback
}

// LimitsMatch keeps total context, maximum input, and maximum output separate.
// Unknown or ambiguous input/output values stay zero rather than being inferred
// by subtracting another published limit.
type LimitsMatch struct {
	Limit      Limit
	Source     string
	ProviderID string
	Stale      bool
}

// LookupRouteOrUnambiguousContext resolves an exact provider/model first.
// Known catalog providers are authoritative: a missing model or limit never
// borrows a sibling route's capacity. If providerID is a friendly custom
// route name absent from the catalog, strictRoute=false allows model-only
// resolution only when every exact model-ID entry has the same positive
// window. No aliases, family prefixes or largest-window guesses are used.
// An explicit catalog_provider should set strictRoute=true, so a typo cannot
// silently select unrelated metadata. This method never performs I/O.
func (c *Client) LookupRouteOrUnambiguousContext(providerID, modelID string, strictRoute bool) (ContextMatch, bool) {
	match, found := c.LookupRouteOrUnambiguousLimits(providerID, modelID, strictRoute)
	if !found {
		return ContextMatch{}, false
	}
	return ContextMatch{Window: match.Limit.Context, Source: match.Source, ProviderID: match.ProviderID, Stale: match.Stale}, true
}

// LookupRouteOrUnambiguousLimits uses the same exact-route and unanimous
// model-ID resolution as LookupRouteOrUnambiguousContext. The total context
// must be positive and unambiguous to accept a match. Input and output are
// independently retained only when every matching route publishes the same
// positive value; omitted or conflicting fields remain unknown (zero).
// Published fields are never derived from one another. Known routes remain
// authoritative, and explicit catalog mappings never borrow another route.
// This method reads only the current snapshot and never performs I/O.
func (c *Client) LookupRouteOrUnambiguousLimits(providerID, modelID string, strictRoute bool) (LimitsMatch, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return LimitsMatch{}, false
	}
	if p, known := c.cached[providerID]; known {
		m, found := p.Models[modelID]
		if !found || m.Limit.Context <= 0 {
			return LimitsMatch{}, false
		}
		limit := m.Limit
		if limit.Input <= 0 {
			limit.Input = 0
		}
		if limit.Output <= 0 {
			limit.Output = 0
		}
		return LimitsMatch{Limit: limit, Source: "catalog_route", ProviderID: providerID, Stale: !c.freshLocked()}, true
	}
	if strictRoute {
		return LimitsMatch{}, false
	}
	limit := Limit{}
	for _, p := range c.cached {
		m, found := p.Models[modelID]
		if !found {
			continue
		}
		// A route that publishes the model but omits its limit cannot
		// participate in a claim that all routes agree on that limit.
		if m.Limit.Context <= 0 || (limit.Context > 0 && limit.Context != m.Limit.Context) {
			return LimitsMatch{}, false
		}
		if limit.Context == 0 {
			limit = m.Limit
			if limit.Input <= 0 {
				limit.Input = 0
			}
			if limit.Output <= 0 {
				limit.Output = 0
			}
			continue
		}
		if m.Limit.Input <= 0 || m.Limit.Input != limit.Input {
			limit.Input = 0
		}
		if m.Limit.Output <= 0 || m.Limit.Output != limit.Output {
			limit.Output = 0
		}
	}
	if limit.Context <= 0 {
		return LimitsMatch{}, false
	}
	return LimitsMatch{Limit: limit, Source: "catalog_model", Stale: !c.freshLocked()}, true
}

// LookupContextWindowByModelID is the provider-agnostic compatibility
// fallback. It succeeds only when every catalog route that publishes the
// model agrees on the same positive limit. Conflicting routes are ambiguous
// and return ok=false rather than guessing a provider or depending on Go map
// iteration order.
func (c *Client) LookupContextWindowByModelID(modelID string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return 0, false
	}
	window := 0
	for _, p := range c.cached {
		if m, ok := p.Models[modelID]; ok && m.Limit.Context > 0 {
			if window == 0 {
				window = m.Limit.Context
				continue
			}
			if window != m.Limit.Context {
				return 0, false
			}
		}
	}
	return window, window > 0
}

// LookupCostByModelID scans every provider for a model whose key
// matches and returns its Cost (USD per million tokens). Same scan
// semantics and caveats as LookupContextWindowByModelID. ok=false
// when the cache is empty, the model is unknown, or the entry has no
// pricing (some Bedrock variants) — callers treat that as "track
// nothing" rather than guessing.
func (c *Client) LookupCostByModelID(modelID string) (Cost, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return Cost{}, false
	}
	for _, p := range c.cached {
		if m, ok := p.Models[modelID]; ok && (m.Cost.Input > 0 || m.Cost.Output > 0) {
			return m.Cost, true
		}
	}
	return Cost{}, false
}

// Stat is a point-in-time snapshot of the catalog's freshness +
// coverage, surfaced via MetisInfo's [catalog] section + the
// `metis models status` CLI. Lets the model self-check "did catalog
// warm-up succeed yet?" without forcing the synchronous Get() path.
type Stat struct {
	CachePath     string    // ~/.metis/cache/models.json (empty if no disk cache resolved)
	CacheBytes    int64     // size on disk; 0 if missing
	CacheModTime  time.Time // mtime; zero if missing
	InMemory      bool      // true once Default()'s warm-up populated c.cached
	LoadedAt      time.Time // when the in-memory copy was loaded; zero if still empty
	SnapshotAt    time.Time // disk modification time or successful network fetch time
	Source        string    // "disk_cache", "network", "static", or empty when cold
	Stale         bool      // missing snapshot or snapshot age exceeds TTL
	ProviderCount int       // providers in the loaded snapshot
	ModelCount    int       // total model entries across all providers
}

// Stat returns a freshness + coverage snapshot. Read-only; safe to
// call from any goroutine, never makes a network request.
func (c *Client) Stat() Stat {
	c.mu.RLock()
	out := Stat{
		CachePath:  c.CachePath,
		LoadedAt:   c.loadedT,
		InMemory:   c.cached != nil,
		SnapshotAt: c.snapshotT,
		Source:     c.source,
		Stale:      !c.freshLocked(),
	}
	if c.cached != nil {
		out.ProviderCount = len(c.cached)
		for _, p := range c.cached {
			out.ModelCount += len(p.Models)
		}
	}
	c.mu.RUnlock()
	if fi, err := os.Stat(out.CachePath); err == nil {
		out.CacheBytes = fi.Size()
		out.CacheModTime = fi.ModTime()
	}
	return out
}

// ModelLocation pairs a provider id with the matching model entry so
// callers can answer "where in the catalog does model X live, and
// what does it claim about itself?". Used by `metis models status
// <model-id>` to surface every catalog row matching the user's id.
type ModelLocation struct {
	ProviderID string
	Model      Model
}

// LookupModel returns every catalog entry for the given model id —
// multiple hits happen when the same model is re-published under a
// regional / proxy provider (e.g. a model appearing under both
// `zai` and `zhipuai`). Returns nil if nothing matches OR if the
// in-memory catalog hasn't loaded yet.
func (c *Client) LookupModel(modelID string) []ModelLocation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || modelID == "" {
		return nil
	}
	var hits []ModelLocation
	for pid, p := range c.cached {
		if m, ok := p.Models[modelID]; ok {
			m.ID = modelID
			hits = append(hits, ModelLocation{ProviderID: pid, Model: m})
		}
	}
	return hits
}

// defaultClient + defaultOnce wire a package-level singleton so the
// provider.MaxContextTokens fast path can call catalog without each
// provider package threading a *Client through its constructor. The
// first call loads disk metadata synchronously, then refreshes expired or
// missing metadata in the background. Offline starts retain cached facts;
// only a missing/invalid disk snapshot remains empty for local fallbacks.
var (
	defaultClient *Client
	defaultOnce   sync.Once
)

// Default returns the process-wide catalog client with existing disk facts
// already available, kicking off a bounded background refresh on first call.
// METIS_HOME selects the same cache home as config and the models command;
// otherwise the standard location is ~/.metis/cache/models.json. Returns nil
// if neither a configured home nor the user's home can be resolved, or if
// METIS_CATALOG_DISABLE=1 is set (test path).
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
		home := os.Getenv("METIS_HOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return
			}
			home = filepath.Join(userHome, ".metis")
		}
		defaultClient = NewClient(home)
		// Background warm-up. 8-second context cap so a hanging
		// upstream doesn't leak a goroutine for hours; the eventual
		// fetch failure is silent — callers retain the disk snapshot,
		// or use local fallback facts if no snapshot could be loaded.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_, _ = defaultClient.Get(ctx)
		}()
	})
	return defaultClient
}

// NewStaticClient returns an in-memory client for deterministic capability
// lookups. It performs no network or disk I/O and is primarily useful to
// callers/tests that already possess catalog facts.
func NewStaticClient(models Catalog) *Client {
	now := time.Now()
	return &Client{cached: models, loadedT: now, snapshotT: now, source: "static"}
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
