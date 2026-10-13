package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
)

const (
	codexModelsURL        = "https://chatgpt.com/backend-api/codex/models?client_version=0.162.0"
	codexContextTTL       = 5 * time.Minute
	codexContextTimeout   = 5 * time.Second
	codexContextBodyLimit = 2 << 20
	codexContextSchema    = 1
)

// CodexContextResolverOptions binds this adapter to the subscription route.
// AccountScope is a startup hint from the persisted OAuth credential; it is
// hashed immediately and is checked against request-time credentials later.
// The catalog is an independent model-spec source, never OAuth discovery.
type CodexContextResolverOptions struct {
	TokenSource  func(context.Context) (ResponsesOAuthCredential, error)
	AccountScope string
	CacheDir     string
	Catalog      *catalog.Client
	// "profile_default" retains the client recommendation. The default uses
	// independently known complete-model limits without inventing new capacity.
	AutoMode string
}

type codexContextProfile struct {
	ID      string `json:"id"`
	Default int    `json:"default"`
	Maximum int    `json:"maximum,omitempty"`
	Input   int    `json:"input,omitempty"`
	Output  int    `json:"output,omitempty"`
}

type codexContextSnapshot struct {
	Schema    int                   `json:"schema"`
	Scope     string                `json:"scope"`
	FetchedAt time.Time             `json:"fetchedAt"`
	Models    []codexContextProfile `json:"models"`
}

// CodexContextResolver keeps status reads entirely memory-only. Refresh is
// called by the agent at a request boundary, and a failed discovery preserves
// the last validated snapshot for that account. Neither credentials nor
// account identifiers appear in the cache, errors, or public capabilities.
type CodexContextResolver struct {
	mu             sync.RWMutex
	refreshGate    chan struct{}
	tokenSource    func(context.Context) (ResponsesOAuthCredential, error)
	cacheDir       string
	catalog        *catalog.Client
	autoMode       string
	http           *http.Client
	now            func() time.Time
	ttl            time.Duration
	activeScope    string
	snapshot       codexContextSnapshot
	credentialHash string
	retryAt        time.Time
	catalogRetryAt time.Time
}

func NewCodexContextResolver(opts CodexContextResolverOptions) *CodexContextResolver {
	c := &CodexContextResolver{tokenSource: opts.TokenSource, cacheDir: opts.CacheDir,
		catalog: opts.Catalog, autoMode: opts.AutoMode, now: time.Now, ttl: codexContextTTL,
		refreshGate: make(chan struct{}, 1),
		http:        &http.Client{Timeout: codexContextTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if opts.AccountScope != "" {
		c.activeScope = codexContextScope(ResponsesOAuthCredential{AccountID: opts.AccountScope})
		c.snapshot = c.load(c.activeScope)
	}
	return c
}

func codexContextScope(credential ResponsesOAuthCredential) string {
	identity := "account:" + credential.AccountID
	if credential.AccountID == "" {
		identity = "credential:" + credential.AccessToken
	}
	digest := sha256.Sum256([]byte(codexModelsURL + "\x00" + identity))
	return hex.EncodeToString(digest[:])
}

func codexCredentialHash(credential ResponsesOAuthCredential) string {
	digest := sha256.Sum256([]byte(credential.AccessToken))
	return hex.EncodeToString(digest[:])
}

// Refresh makes one bounded GET to the fixed subscription origin. It does not
// issue model generation, public API model listing, or redirected requests.
func (c *CodexContextResolver) Refresh(ctx context.Context) error {
	if c == nil || c.tokenSource == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
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
	credential, err := c.tokenSource(ctx)
	if err != nil || strings.TrimSpace(credential.AccessToken) == "" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, llm.ErrContextBudgetPolicy) {
			return fmt.Errorf("%w: Codex account binding unavailable", llm.ErrContextBudgetPolicy)
		}
		// Authentication errors are not generation errors. Do not repeat a
		// callback's possibly credential-bearing error text at this boundary.
		return nil
	}
	scope, fingerprint := codexContextScope(credential), codexCredentialHash(credential)
	now := c.now()
	c.mu.Lock()
	if c.activeScope != scope {
		c.activeScope, c.snapshot, c.retryAt = scope, c.load(scope), time.Time{}
		c.credentialHash = ""
	}
	unchangedCredential := c.credentialHash == fingerprint
	fresh := unchangedCredential && c.snapshot.Schema == codexContextSchema && c.ttl > 0 && now.Sub(c.snapshot.FetchedAt) < c.ttl
	backoff := unchangedCredential && now.Before(c.retryAt)
	c.credentialHash = fingerprint
	c.mu.Unlock()
	if !fresh && !backoff {
		requestCtx, cancel := context.WithTimeout(ctx, codexContextTimeout)
		models, fetchErr := c.fetch(requestCtx, credential)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.mu.Lock()
		if fetchErr == nil {
			c.snapshot = codexContextSnapshot{Schema: codexContextSchema, Scope: scope, FetchedAt: now, Models: models}
			c.retryAt = time.Time{}
		} else {
			c.retryAt = now.Add(30 * time.Second)
		}
		snapshot := c.snapshot
		c.mu.Unlock()
		if fetchErr == nil {
			c.persist(snapshot)
		}
	}
	// Catalog refresh is independent and secret-free. It is allowed only at
	// this request boundary; status reads use its previous cached snapshot.
	if c.catalog != nil && c.catalog.Stat().Stale && !now.Before(c.catalogRetryAt) {
		catalogCtx, cancel := context.WithTimeout(ctx, codexContextTimeout)
		_, _ = c.catalog.Get(catalogCtx)
		cancel()
		if c.catalog.Stat().Stale {
			c.catalogRetryAt = now.Add(30 * time.Second)
		} else {
			c.catalogRetryAt = time.Time{}
		}
	}
	return ctx.Err()
}

func (c *CodexContextResolver) fetch(ctx context.Context, credential ResponsesOAuthCredential) ([]codexContextProfile, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, codexModelsURL, nil)
	if err != nil {
		return nil, errors.New("codex context request unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	if credential.AccountID != "" {
		request.Header.Set("chatgpt-account-id", credential.AccountID)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "metis")
	request.Header.Set("originator", "metis")
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("codex context discovery unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex context discovery HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, codexContextBodyLimit+1))
	if err != nil || len(body) > codexContextBodyLimit {
		return nil, errors.New("codex context discovery invalid body")
	}
	return parseCodexContextProfiles(body)
}

func parseCodexContextProfiles(body []byte) ([]codexContextProfile, error) {
	var wire struct {
		Models []struct {
			Slug      string `json:"slug"`
			Context   int    `json:"context_window"`
			Maximum   int    `json:"max_context_window"`
			Input     int    `json:"max_input_tokens"`
			Output    int    `json:"max_output_tokens"`
			MaxTokens int    `json:"max_tokens"`
		} `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&wire); err != nil {
		return nil, errors.New("codex context discovery invalid schema")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("codex context discovery trailing data")
	}
	if len(wire.Models) == 0 || len(wire.Models) > 10000 {
		return nil, errors.New("codex context discovery invalid model count")
	}
	seen := make(map[string]bool, len(wire.Models))
	models := make([]codexContextProfile, 0, len(wire.Models))
	for _, item := range wire.Models {
		profile := codexContextProfile{ID: strings.TrimSpace(item.Slug), Default: item.Context, Maximum: item.Maximum, Input: item.Input, Output: item.Output}
		if profile.Output == 0 {
			profile.Output = item.MaxTokens
		}
		if !validCodexContextProfile(profile) || seen[profile.ID] {
			return nil, errors.New("codex context discovery invalid model limits")
		}
		seen[profile.ID] = true
		models = append(models, profile)
	}
	return models, nil
}

func validCodexContextProfile(p codexContextProfile) bool {
	if p.ID == "" || len(p.ID) > 512 || strings.ContainsAny(p.ID, "\r\n\x00") {
		return false
	}
	if p.Default < 0 || p.Default > 100_000_000 || p.Maximum < 0 || p.Maximum > 100_000_000 || (p.Maximum > 0 && p.Maximum < p.Default) {
		return false
	}
	return p.Input >= 0 && p.Input <= 100_000_000 && p.Output >= 0 && p.Output <= 100_000_000
}

// Resolve distinguishes full model capacity from client recommendations. No
// profile maximum becomes a physical limit, no model prefixes or aliases are
// accepted, and independent input/output limits are never added together.
func (c *CodexContextResolver) Resolve(model string, configured int, fallback func() int) llm.ContextCapabilities {
	window := fallback()
	out := llm.ContextCapabilities{Window: window, Source: "subscription_model_estimate", WindowKind: "total"}
	c.mu.RLock()
	for _, profile := range c.snapshot.Models {
		if profile.ID != model {
			continue
		}
		out.ProfileDefaultWindow, out.ProfileMaxWindow = profile.Default, profile.Maximum
		if profile.Default > 0 {
			out.Window, out.Source = profile.Default, "codex_subscription_models"
		}
		out.MaxInputTokens, out.MaxOutputTokens = profile.Input, profile.Output
		break
	}
	c.mu.RUnlock()
	if c.catalog != nil {
		// An exact OpenAI model specification can describe complete model
		// capacity without borrowing the public API's authentication route.
		if match, known := c.catalog.LookupRouteOrUnambiguousLimits("openai", model, true); known {
			out.ModelWindow, out.ModelMaxInputTokens, out.ModelMaxOutputTokens = match.Limit.Context, match.Limit.Input, match.Limit.Output
			out.ModelSource, out.MaxWindow, out.LimitSource = "codex_model_catalog", match.Limit.Context, "codex_model_catalog"
			if c.autoMode != "profile_default" {
				out.Window, out.Source = match.Limit.Context, "codex_model_catalog"
			}
			out.MaxInputTokens = codexMinimumLimit(out.MaxInputTokens, match.Limit.Input)
			out.MaxOutputTokens = codexMinimumLimit(out.MaxOutputTokens, match.Limit.Output)
		}
	}
	if configured > 0 {
		out.RequestedWindow, out.Window, out.Source = configured, configured, "configured"
		if out.MaxWindow > 0 && out.Window > out.MaxWindow {
			out.Window, out.Source = out.MaxWindow, "configured_clamped"
		}
	}
	return out
}

func codexMinimumLimit(first, second int) int {
	if first <= 0 {
		return second
	}
	if second <= 0 {
		return first
	}
	return min(first, second)
}

// Codex does not accept max_output_tokens on its wire protocol. Still reject
// an impossible local output allocation rather than pretending it fits the
// declared model budget or sending an unsupported parameter to the server.
func (r *Responses) validateCodexContextOutputBudget(request llm.Request) error {
	if r.Name() != "openai-codex" {
		return nil
	}
	maximum := r.ContextCapabilities().MaxOutputTokens
	requested := r.MaxTokens
	if request.MaxTokens > 0 {
		requested = request.MaxTokens
	}
	if maximum > 0 && requested > maximum {
		return fmt.Errorf("%w: requested %d, maximum %d", llm.ErrContextOutputBudget, requested, maximum)
	}
	return nil
}

func (c *CodexContextResolver) load(scope string) codexContextSnapshot {
	if c.cacheDir == "" || scope == "" {
		return codexContextSnapshot{}
	}
	file, err := os.Open(filepath.Join(c.cacheDir, scope+".json"))
	if err != nil {
		return codexContextSnapshot{}
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, codexContextBodyLimit+1))
	var snapshot codexContextSnapshot
	if err != nil || len(body) > codexContextBodyLimit || json.Unmarshal(body, &snapshot) != nil || snapshot.Schema != codexContextSchema || snapshot.Scope != scope || snapshot.FetchedAt.IsZero() || len(snapshot.Models) == 0 || len(snapshot.Models) > 10000 {
		return codexContextSnapshot{}
	}
	seen := make(map[string]bool, len(snapshot.Models))
	for _, model := range snapshot.Models {
		if !validCodexContextProfile(model) || seen[model.ID] {
			return codexContextSnapshot{}
		}
		seen[model.ID] = true
	}
	return snapshot
}

func (c *CodexContextResolver) persist(snapshot codexContextSnapshot) {
	if c.cacheDir == "" {
		return
	}
	if os.MkdirAll(c.cacheDir, 0o700) != nil {
		return
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	file, err := os.CreateTemp(c.cacheDir, ".codex-context-*")
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
	_ = os.Rename(name, filepath.Join(c.cacheDir, snapshot.Scope+".json"))
}
