package gemini

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const (
	geminiContextTTL       = 5 * time.Minute
	geminiContextTimeout   = 5 * time.Second
	geminiContextBodyLimit = 1 << 20
)

var ErrContextOutputBudget = llm.ErrContextOutputBudget

func (g *Gemini) validateContextOutputBudget(request Request) error {
	maximum := g.ContextCapabilities().MaxOutputTokens
	requested := g.MaxTokens
	if request.MaxTokens > 0 {
		requested = request.MaxTokens
	}
	if maximum > 0 && requested > maximum {
		return fmt.Errorf("%w: requested %d, maximum %d", ErrContextOutputBudget, requested, maximum)
	}
	return nil
}

type ContextDiscoveryOptions struct {
	BaseURL  string
	Model    string
	APIKey   string
	CacheDir string
}

type geminiContextSnapshot struct {
	Route     string    `json:"route"`
	Model     string    `json:"model"`
	Input     int       `json:"input"`
	Output    int       `json:"output,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// ContextDiscovery reads exactly the model used by generation on the same
// configured origin. It retains independently reported input/output limits;
// their sum is never promoted into an invented total context window.
type ContextDiscovery struct {
	mu                                    sync.RWMutex
	refreshGate                           chan struct{}
	baseURL, model, apiKey, cacheDir, key string
	http                                  *http.Client
	now                                   func() time.Time
	ttl                                   time.Duration
	snapshot                              geminiContextSnapshot
	retryAt                               time.Time
}

func NewContextDiscovery(opts ContextDiscoveryOptions) *ContextDiscovery {
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	digest := sha256.Sum256([]byte("gemini_native\x00" + baseURL + "\x00" + opts.Model))
	c := &ContextDiscovery{baseURL: baseURL, model: opts.Model, apiKey: opts.APIKey, cacheDir: opts.CacheDir,
		key: hex.EncodeToString(digest[:]), now: time.Now, ttl: geminiContextTTL,
		refreshGate: make(chan struct{}, 1),
		http:        &http.Client{Timeout: geminiContextTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	c.snapshot = c.load()
	return c
}

func (g *Gemini) ContextCapabilities() (resolved llm.ContextCapabilities) {
	defer func() { resolved = g.ContextBudgetPolicy.Apply(resolved) }()
	configuredWindow, budgetSource := g.ContextWindow, g.ContextBudgetSource
	if g.ContextBudgetPolicy != nil {
		configuredWindow, budgetSource = g.ContextBudgetPolicy.Current()
	}
	out := llm.ContextCapabilities{Window: g.fallbackContextWindow(), WindowKind: llm.ContextWindowTotal, Source: "estimated"}
	if g.ContextDiscovery != nil {
		out = g.ContextDiscovery.resolve(out)
	}
	if configuredWindow > 0 {
		out.RequestedWindow, out.Window, out.Source = configuredWindow, configuredWindow, "configured"
		// Native Gemini reports an input ceiling, not a combined total.
		if out.MaxInputTokens > 0 && out.Window > out.MaxInputTokens {
			out.Window, out.Source = out.MaxInputTokens, "configured_clamped"
		}
	}
	return llm.ApplyContextBudgetSource(out, budgetSource)
}

func (g *Gemini) RefreshContextCapabilities(ctx context.Context) error {
	if g.ContextBudgetPolicy != nil {
		if err := g.ContextBudgetPolicy.Refresh(ctx); err != nil {
			return err
		}
	}
	if g.ContextDiscovery == nil {
		return nil
	}
	return g.ContextDiscovery.Refresh(ctx)
}

func (c *ContextDiscovery) resolve(out llm.ContextCapabilities) llm.ContextCapabilities {
	c.mu.RLock()
	snapshot := c.snapshot
	c.mu.RUnlock()
	if snapshot.Input > 0 {
		out.Window, out.MaxInputTokens, out.MaxOutputTokens = snapshot.Input, snapshot.Input, snapshot.Output
		out.WindowKind, out.Source, out.LimitSource = llm.ContextWindowInput, "gemini_model_input", "gemini_model_input"
		out.ModelMaxInputTokens, out.ModelMaxOutputTokens, out.ModelSource = snapshot.Input, snapshot.Output, "gemini_model_input"
	}
	return out
}

func (c *ContextDiscovery) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.apiKey == "" {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.refreshGate <- struct{}{}:
	}
	defer func() { <-c.refreshGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.now()
	c.mu.RLock()
	fresh := c.snapshot.Input > 0 && c.ttl > 0 && now.Sub(c.snapshot.FetchedAt) < c.ttl
	backoff := now.Before(c.retryAt)
	c.mu.RUnlock()
	if fresh || backoff {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, geminiContextTimeout)
	snapshot, err := c.fetch(requestCtx)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	if err == nil {
		c.snapshot, c.retryAt = snapshot, time.Time{}
	} else {
		c.retryAt = now.Add(30 * time.Second)
	}
	c.mu.Unlock()
	if err == nil {
		c.persist(snapshot)
	}
	return nil
}

func (c *ContextDiscovery) fetch(ctx context.Context) (geminiContextSnapshot, error) {
	empty := geminiContextSnapshot{}
	base, err := url.Parse(c.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return empty, errors.New("gemini context discovery invalid origin")
	}
	model := c.model
	// The generation transport sends Model verbatim after /models/. Do not
	// strip aliases or normalize a different wire selection for discovery.
	if !validGeminiDiscoveryModel(model) {
		return empty, errors.New("gemini context discovery invalid model")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1beta/models/" + model
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return empty, errors.New("gemini context discovery invalid request")
	}
	request.Header.Set("x-goog-api-key", c.apiKey)
	request.Header.Set("Accept", "application/json")
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return empty, errors.New("gemini context discovery unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return empty, errors.New("gemini context discovery rejected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, geminiContextBodyLimit+1))
	if err != nil || len(body) > geminiContextBodyLimit {
		return empty, errors.New("gemini context discovery invalid body")
	}
	var wire struct {
		Name   string `json:"name"`
		Input  int    `json:"inputTokenLimit"`
		Output int    `json:"outputTokenLimit"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Name != "models/"+model || wire.Input <= 0 || wire.Input > 100_000_000 || wire.Output < 0 || wire.Output > 100_000_000 {
		return empty, errors.New("gemini context discovery invalid model limits")
	}
	return geminiContextSnapshot{Route: c.key, Model: c.model, Input: wire.Input, Output: wire.Output, FetchedAt: c.now()}, nil
}

func validGeminiDiscoveryModel(model string) bool {
	if model == "" || len(model) > 512 {
		return false
	}
	for _, r := range model {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func (c *ContextDiscovery) load() geminiContextSnapshot {
	if c.cacheDir == "" {
		return geminiContextSnapshot{}
	}
	file, err := os.Open(filepath.Join(c.cacheDir, c.key+".json"))
	if err != nil {
		return geminiContextSnapshot{}
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, geminiContextBodyLimit+1))
	var snapshot geminiContextSnapshot
	if err != nil || len(body) > geminiContextBodyLimit || json.Unmarshal(body, &snapshot) != nil || snapshot.Route != c.key || snapshot.Model != c.model || snapshot.Input <= 0 || snapshot.Input > 100_000_000 || snapshot.Output < 0 || snapshot.Output > 100_000_000 || snapshot.FetchedAt.IsZero() {
		return geminiContextSnapshot{}
	}
	return snapshot
}

func (c *ContextDiscovery) persist(snapshot geminiContextSnapshot) {
	if c.cacheDir == "" || os.MkdirAll(c.cacheDir, 0o700) != nil {
		return
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	file, err := os.CreateTemp(c.cacheDir, ".gemini-context-*")
	if err != nil {
		return
	}
	name := file.Name()
	defer os.Remove(name)
	if file.Chmod(0o600) != nil {
		_ = file.Close()
		return
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return
	}
	if file.Close() != nil {
		return
	}
	_ = os.Rename(name, filepath.Join(c.cacheDir, c.key+".json"))
}

var _ llm.ContextCapabilitiesProvider = (*Gemini)(nil)
