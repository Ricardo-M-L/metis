// Package openai implements the OpenAI Chat Completions API
// (api.openai.com/v1/chat/completions and OpenAI-compatible gateways:
// DeepSeek, Together, Groq, Cerebras, MiniMax /v1, Ollama …). Used
// directly when transport=openai_chat, and consumed by the azure
// subpackage which runs the same wire format with deployment-routed URLs.
package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/sse"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	pubLLM "github.com/Ricardo-M-L/metis/pkg/llm"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type (
	Request       = provider.Request
	Response      = provider.Response
	StreamReader  = provider.StreamReader
	StreamEvent   = provider.StreamEvent
	Message       = provider.Message
	ContentBlock  = provider.ContentBlock
	ToolSpec      = provider.ToolSpec
	SystemSection = provider.SystemSection
	Effort        = pubLLM.Effort
)

const (
	RoleSystem    = provider.RoleSystem
	RoleUser      = provider.RoleUser
	RoleAssistant = provider.RoleAssistant
	RoleTool      = provider.RoleTool

	EffortDefault = pubLLM.EffortDefault
	EffortLow     = pubLLM.EffortLow
	EffortMedium  = pubLLM.EffortMedium
	EffortHigh    = pubLLM.EffortHigh
)

var dbgOpenAI = os.Getenv("METIS_DEBUG_OPENAI") == "1"

// OpenAI implements Provider against /v1/chat/completions.
// Compatible with any OpenAI-Chat-style endpoint (Together, Groq, Ollama, etc.)
// by overriding BaseURL.
type OpenAI struct {
	APIKey      string
	BaseURL     string
	Model       string
	MaxTokens   int
	Temperature float64
	// ContextWindow, when > 0, overrides the model-prefix lookup in
	// MaxContextTokens(). Useful for OpenAI-compatible gateways
	// (Together, Groq, Ollama) where the served model is a fine-tune
	// whose name doesn't match a known prefix.
	ContextWindow int
	httpClient    *http.Client
	// requestSlots is shared by the parent loop and every sub-agent using
	// this provider instance. Holding a slot until a streaming body closes
	// prevents an 8-agent fan-out from opening 8 simultaneous generations
	// against the same OpenAI-compatible RPM bucket.
	requestSlots chan struct{}
	// rateUntil is a provider-instance-wide 429 cooldown. The parent loop and
	// all Agent children share this OpenAI pointer, so one RPM response pauses
	// requests that have not reached the wire yet instead of letting every
	// child independently discover the same exhausted bucket.
	rateMu      sync.Mutex
	rateUntil   time.Time
	rateLastHit time.Time
	rateStrikes int
}

func New(apiKey, baseURL, model string, maxTokens int, timeout time.Duration, temperature float64) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}
	return &OpenAI{
		APIKey:       apiKey,
		BaseURL:      strings.TrimRight(baseURL, "/"),
		Model:        model,
		MaxTokens:    maxTokens,
		Temperature:  temperature,
		httpClient:   transport.NewHTTPClient(timeout, "openai"),
		requestSlots: newOpenAIRequestSlots(),
	}
}

const defaultOpenAIConcurrency = 4

func newOpenAIRequestSlots() chan struct{} {
	limit := defaultOpenAIConcurrency
	if raw := strings.TrimSpace(os.Getenv("METIS_OPENAI_MAX_CONCURRENCY")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

func (o *OpenAI) acquireRequestSlot(ctx context.Context) (func(), error) {
	if o.requestSlots == nil {
		return func() {}, nil
	}
	select {
	case o.requestSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-o.requestSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (o *OpenAI) waitRateCooldown(ctx context.Context) error {
	for {
		o.rateMu.Lock()
		remaining := time.Until(o.rateUntil)
		o.rateMu.Unlock()
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			// Go 1.23+ timer channels are synchronous. A failed Stop no
			// longer implies that a value is available to drain, so a
			// blocking receive here can park cancellation forever.
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Another concurrent 429 may have extended rateUntil while this
			// timer slept. Loop and re-read instead of firing early.
		}
	}
}

// noteRateLimit records a shared cooldown and returns the delay this request
// should attach to RetryableError.After. Without a server Retry-After,
// concurrent RPM failures escalate 5s -> 10s -> 20s -> 30s (capped), which
// turns an 8-agent burst into one bounded cool-down rather than 8 retry loops.
func (o *OpenAI) noteRateLimit(after time.Duration) time.Duration {
	now := time.Now()
	o.rateMu.Lock()
	defer o.rateMu.Unlock()
	if o.rateLastHit.IsZero() || now.Sub(o.rateLastHit) > time.Minute {
		o.rateStrikes = 0
	}
	o.rateLastHit = now
	o.rateStrikes++
	if after <= 0 {
		shift := o.rateStrikes - 1
		if shift > 3 {
			shift = 3
		}
		after = time.Duration(1<<uint(shift)) * 5 * time.Second
		if after > 30*time.Second {
			after = 30 * time.Second
		}
	}
	if after > 60*time.Second {
		after = 60 * time.Second
	}
	if until := now.Add(after); until.After(o.rateUntil) {
		o.rateUntil = until
	}
	// A sibling may already have installed a longer cooldown. Make this
	// request honor the shared deadline too, not merely its local response.
	return time.Until(o.rateUntil)
}

func (o *OpenAI) noteRateSuccess() {
	now := time.Now()
	o.rateMu.Lock()
	if !now.Before(o.rateUntil) {
		o.rateStrikes = 0
	}
	o.rateMu.Unlock()
}

// MaxContextTokens resolves the active model's context window through
// a 4-tier fallback chain. Each tier is more vendor-specific than the
// last so the cheap-and-correct path wins:
//
//  1. User override — `[provider.<name>].context_window = N` in
//     ~/.metis/config.toml. Highest priority because a self-hosted
//     gateway or capped account might serve a smaller slice than the
//     public model card claims.
//  2. models.dev catalog — synchronous read of the in-memory cache
//     populated by the background warm-up (catalog.Default()). Covers
//     117 providers' published windows; updates as new models ship.
//  3. Hardcoded prefix table — vendor-published numbers for the
//     models metis users actually run today. Belt to catalog's braces
//     so a cold start / offline session still picks the right window
//     within microseconds.
//  4. `-Nk` / `-Nm` suffix parsing — DeepSeek-TUI convention. Lets a
//     brand-new variant (or a self-host fork) signal its window in
//     the model id when nothing else knows about it yet.
//
// All tiers fall through to 128_000 — a safe modern default that
// almost certainly under-counts rather than over-counts. Over-counting
// would let compaction fire late and risk a 4xx; under-counting just
// compacts sooner than strictly necessary.
// ModelID returns the wire-level model id this provider sends, for
// trustworthy status-bar display.
func (o *OpenAI) ModelID() string { return o.Model }

// SupportsVision reports whether the configured OpenAI model accepts
// image_url content parts.
//
// 2026-08-01 rework: explicit facts + catalog + whitelist. The previous
// implementation was only a hardcoded prefix list. We retain confirmed
// provider facts for deterministic offline behavior, then ask models.dev for
// ids not covered locally.
//
// Capability is tri-state: known text-only stays Unsupported, catalog/model
// misses remain Unknown, and callers let unknown custom gateways adjudicate.
func (o *OpenAI) SupportsVision() bool {
	return o.VisionCapability() == provider.VisionSupported
}

func (o *OpenAI) VisionCapability() provider.VisionCapability {
	return VisionCapabilityForModel(o.Model)
}

// SupportsVisionModel exposes the same catalog-first capability decision for
// configured-profile pickers that need to filter candidates before building a
// live provider. It performs no request and requires no API key.
func SupportsVisionModel(model string) bool {
	return VisionCapabilityForModel(model) == provider.VisionSupported
}

// VisionCapabilityForModel distinguishes an explicit text-only catalog entry
// from a model the local catalog simply does not know. Image submission gates
// only reject the former; unknown custom gateways get a chance to let their
// own API make the authoritative decision.
func VisionCapabilityForModel(model string) provider.VisionCapability {
	m := strings.ToLower(strings.TrimSpace(model))

	// Vendor-confirmed exact ids take precedence over the shared catalog.
	// SenseNova's authenticated /models metadata declares text+image input for
	// this model, while models.dev does not currently carry the vendor entry.
	// Keeping this exact (rather than a broad sensenova-* prefix) avoids
	// accidentally sending images to text-only siblings.
	if m == "sensenova-6.8-flash-lite" {
		return provider.VisionSupported
	}
	return visionCapabilityWithCatalog(model, m, catalog.Default())
}

func visionCapabilityWithCatalog(model, normalized string, cli *catalog.Client) provider.VisionCapability {
	// Tier 1 — models.dev. A positive or negative catalog fact is stronger
	// than broad family heuristics below. A miss falls through to the offline
	// compatibility table.
	if cli != nil {
		if supported, found := cli.LookupVisionByModelID(model); found {
			if supported {
				return provider.VisionSupported
			}
			return provider.VisionUnsupported
		}
	}
	return fallbackVisionCapability(normalized)
}

// fallbackVisionCapability contains only deterministic cold-cache heuristics.
// Keeping it separate lets unit tests exercise this table without depending
// on a user's mutable ~/.metis catalog or a background network refresh.
func fallbackVisionCapability(m string) provider.VisionCapability {
	// Tier 2 — cold-cache/offline family facts.
	switch {
	// OpenAI native lineage.
	case strings.HasPrefix(m, "gpt-4o"),
		strings.HasPrefix(m, "gpt-5"),
		strings.HasPrefix(m, "gpt-4.1"),
		strings.HasPrefix(m, "gpt-4-vision"),
		strings.HasPrefix(m, "gpt-4-turbo"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"),
		strings.HasPrefix(m, "chatgpt-4o"):
		return provider.VisionSupported
	// Chinese OSS families (fallback only — prefer catalog above).
	case strings.HasPrefix(m, "deepseek-vl"),
		strings.HasPrefix(m, "kimi-k2"),
		strings.HasPrefix(m, "kimi-k3"),
		strings.HasPrefix(m, "kimi-latest"),
		strings.HasPrefix(m, "kimi-vl"),
		strings.HasPrefix(m, "moonshot-v1-vision"),
		strings.HasPrefix(m, "glm-5"),
		strings.HasPrefix(m, "glm-4v"),
		strings.HasPrefix(m, "qwen-vl"),
		strings.HasPrefix(m, "qwen2.5-vl"):
		return provider.VisionSupported
	}

	// Confirmed text-only lineages retained from the pre-tristate table.
	// Everything else is Unknown: absence from this client is not evidence of
	// an upstream limitation.
	switch {
	case strings.HasPrefix(m, "gpt-3.5"),
		m == "gpt-4",
		strings.HasPrefix(m, "text-davinci"),
		strings.HasPrefix(m, "deepseek-v3"),
		strings.HasPrefix(m, "deepseek-v4"),
		strings.HasPrefix(m, "deepseek-chat"),
		strings.HasPrefix(m, "deepseek-reasoner"),
		strings.HasPrefix(m, "ark-code"),
		strings.HasPrefix(m, "kimi-k1.5"),
		strings.HasPrefix(m, "glm-4-flash"),
		strings.HasPrefix(m, "minimax-m"):
		return provider.VisionUnsupported
	}

	return provider.VisionUnknown
}

func (o *OpenAI) MaxContextTokens() int {
	// Tier 1 — explicit user override.
	if o.ContextWindow > 0 {
		return o.ContextWindow
	}

	// Tier 2 — models.dev catalog. nil-safe (Default() returns nil
	// when HOME is unset, e.g. CI minimal env) and miss-safe (returns
	// false until the background fetch completes).
	if cli := catalog.Default(); cli != nil {
		if w, ok := cli.LookupContextWindowByModelID(o.Model); ok {
			return w
		}
	}

	// Tier 3 — `-Nk` / `-Nm` suffix. Runs BEFORE the prefix table so
	// a vendor variant like "moonshot-v1-32k" gets its declared 32K
	// instead of being captured by the generic "moonshot" prefix and
	// served the wrong (default 200K) window. The suffix is always
	// more specific than the family prefix when both match.
	if w, ok := transport.ParseModelWindowSuffix(o.Model); ok {
		return w
	}

	// Tier 4 — hardcoded vendor family table. Ordered most-specific
	// first so "deepseek-v4-pro" matches the v4 row instead of the
	// generic deepseek row. The numbers here track vendor-published
	// model cards as of 2026-05-16; bump them when a vendor publishes
	// a new card AND catalog hasn't picked it up yet.
	switch {
	// OpenAI native
	case strings.HasPrefix(o.Model, "o1"), strings.HasPrefix(o.Model, "o3"):
		return 200_000 // o-series reasoning models
	case strings.HasPrefix(o.Model, "gpt-4o"):
		return 128_000
	case strings.HasPrefix(o.Model, "gpt-4-turbo"):
		return 128_000
	case strings.HasPrefix(o.Model, "gpt-4-32k"):
		return 32_768
	case strings.HasPrefix(o.Model, "gpt-4"):
		return 128_000
	case strings.HasPrefix(o.Model, "gpt-3.5-turbo-16k"):
		return 16_385
	case strings.HasPrefix(o.Model, "gpt-3.5-turbo"):
		return 16_385

	// DeepSeek
	case strings.HasPrefix(o.Model, "deepseek-v4"), strings.HasPrefix(o.Model, "DeepSeek-V4"):
		return 1_000_000 // v4-pro 1M
	case strings.HasPrefix(o.Model, "deepseek-v3"), strings.HasPrefix(o.Model, "DeepSeek-V3"):
		return 128_000
	case strings.HasPrefix(o.Model, "deepseek"), strings.HasPrefix(o.Model, "DeepSeek"):
		return 128_000 // deepseek-chat / deepseek-coder default

	// Kimi / Moonshot (Singapore + global)
	case strings.HasPrefix(o.Model, "kimi-k2"), strings.HasPrefix(o.Model, "Kimi-K2"):
		return 200_000
	case strings.HasPrefix(o.Model, "kimi"), strings.HasPrefix(o.Model, "Kimi"):
		return 200_000
	case strings.HasPrefix(o.Model, "moonshot"), strings.HasPrefix(o.Model, "Moonshot"):
		// Variants with `-Nk` suffix (e.g. moonshot-v1-32k) get
		// caught by tier 3 before reaching this row — so this
		// generic moonshot prefix only fires on the bare family name.
		return 200_000

	// GLM / Zhipu — Zhipu bumped the window family-by-family. 4.0
	// through 4.5 stayed at 128k; 4.6 doubled to 200k; 4.7 / Flash
	// hold that; 5.x ships at 200k. Order matters: more-specific
	// versions match before the GLM-4 catch-all so a 4.6 / 4.7 id
	// doesn't fall into the legacy 128k bucket.
	case strings.HasPrefix(o.Model, "glm-5"), strings.HasPrefix(o.Model, "GLM-5"):
		return 200_000
	case strings.HasPrefix(o.Model, "glm-4.7"), strings.HasPrefix(o.Model, "GLM-4.7"),
		strings.HasPrefix(o.Model, "glm-4.6"), strings.HasPrefix(o.Model, "GLM-4.6"):
		return 200_000
	case strings.HasPrefix(o.Model, "glm-4-plus"), strings.HasPrefix(o.Model, "GLM-4-Plus"):
		return 128_000
	case strings.HasPrefix(o.Model, "glm-4"), strings.HasPrefix(o.Model, "GLM-4"):
		return 128_000
	case strings.HasPrefix(o.Model, "glm"), strings.HasPrefix(o.Model, "GLM"):
		return 128_000

	// MiniMax (rare on openai_chat transport but possible via custom shim)
	case strings.HasPrefix(o.Model, "MiniMax"), strings.HasPrefix(o.Model, "minimax"):
		return 200_000

	// Qwen / Aliyun DashScope
	case strings.HasPrefix(o.Model, "qwen3-235b"), strings.HasPrefix(o.Model, "Qwen3-235B"):
		return 256_000
	case strings.HasPrefix(o.Model, "qwen2.5"), strings.HasPrefix(o.Model, "Qwen2.5"):
		return 128_000
	case strings.HasPrefix(o.Model, "qwen"), strings.HasPrefix(o.Model, "Qwen"):
		return 128_000

	// Mistral
	case strings.HasPrefix(o.Model, "mistral-large"), strings.HasPrefix(o.Model, "Mistral-Large"):
		return 128_000
	case strings.HasPrefix(o.Model, "codestral"):
		return 256_000
	case strings.HasPrefix(o.Model, "mistral-medium"), strings.HasPrefix(o.Model, "Mistral-Medium"):
		return 32_000
	case strings.HasPrefix(o.Model, "mistral-small"), strings.HasPrefix(o.Model, "Mistral-Small"):
		return 32_000
	case strings.HasPrefix(o.Model, "mistral"), strings.HasPrefix(o.Model, "Mistral"):
		return 32_000

	// Llama (typically self-hosted via Ollama / Together / Groq)
	case strings.HasPrefix(o.Model, "llama-3.3"), strings.HasPrefix(o.Model, "Llama-3.3"):
		return 128_000
	case strings.HasPrefix(o.Model, "llama-3.1"), strings.HasPrefix(o.Model, "Llama-3.1"):
		return 128_000
	case strings.HasPrefix(o.Model, "llama-3"), strings.HasPrefix(o.Model, "Llama-3"):
		return 8_192
	case strings.HasPrefix(o.Model, "llama-4"), strings.HasPrefix(o.Model, "Llama-4"):
		return 10_000_000 // Scout/Maverick announced 10M
	}

	return 128_000 // ultimate safe default
}

func (o *OpenAI) Name() string { return "openai" }

// --- request shapes ---

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type oaiMessage struct {
	Role string `json:"role"`
	// Content is `any` because OpenAI accepts EITHER a string (plain
	// text — the historical 99% of metis traffic) OR a content-parts
	// array `[{"type":"text",...},{"type":"image_url",...}]` for
	// multimodal user turns. Marshalling distinguishes via the
	// underlying type.
	Content    any           `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`

	// ReasoningContent is Moonshot/Kimi's "thinking" extension to the
	// OpenAI Chat Completions wire format. When `kimi-k2-thinking-*`
	// is in play, Moonshot's API REQUIRES that any assistant message
	// containing tool_calls also carry a reasoning_content field —
	// otherwise the next turn 400s with
	// "thinking is enabled but reasoning_content is missing in
	// assistant tool call message at index N".
	//
	// Pointer type so nil omits the JSON key entirely; an empty
	// string emits `"reasoning_content": ""`. We only set non-nil on
	// assistant tool-call messages — that's the only path Moonshot
	// validates, and other OpenAI-compatible providers ignore unknown
	// keys so it doesn't break OpenAI / DeepSeek / Together / Groq.
	//
	// The receive path maps this field to provider-neutral "thinking"
	// blocks/events, so DeepSeek/Ark/Kimi reasoning is visible in the TUI
	// and round-trips faithfully on the next tool iteration.
	ReasoningContent *string `json:"reasoning_content,omitempty"`

	// Reasoning is the newer alias emitted by vLLM/OpenRouter-compatible
	// Chat Completions endpoints. We accept it on responses but always send
	// history back through the widely-supported reasoning_content field above.
	Reasoning *string `json:"reasoning,omitempty"`
}

// reasoningText normalizes the two Chat Completions reasoning extensions.
// Some gateways send both aliases with identical bytes, so select the first
// non-empty value instead of concatenating and showing the trace twice.
func (m oaiMessage) reasoningText() string {
	for _, candidate := range []*string{m.ReasoningContent, m.Reasoning} {
		if candidate != nil && *candidate != "" {
			return *candidate
		}
	}
	return ""
}

// oaiContentPart is one element of a multimodal user-message Content
// array. Type is "text" or "image_url". OpenAI / DeepSeek / Moonshot
// (Kimi) all accept this shape; vision-incapable models will simply
// 400 — metis surfaces that as a turn-level error rather than
// silently dropping pasted images.
type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiReq struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	// ReasoningEffort is recognized by OpenAI o1 / o3 / GPT-5-style
	// reasoning models (and forwarded by most OpenAI-compat gateways).
	// Empty string means: don't send the field. Non-reasoning models
	// generally ignore it but a few error out, so omit by default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	StreamOptions   *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

// oaiUsage captures the three wire shapes for cached-prefix tokens
// seen on OpenAI-compatible providers:
//
//   - OpenAI / GLM / Zhipu / Gemini-compat:
//     `prompt_tokens_details.cached_tokens` (nested)
//   - DeepSeek:
//     `prompt_cache_hit_tokens` (flat, sibling of prompt_tokens)
//   - Kimi (Moonshot) and some misc:
//     `cached_tokens` (flat)
//
// The provider keeps reading whichever field its upstream chose to
// emit; cacheReadTokens() picks the first non-zero value as the
// canonical "cached input tokens this turn" number, which then maps
// to StreamEvent.CacheReadInputTokens / Response.CacheReadInputTokens
// (the same field Anthropic's cache_read_input_tokens lands in).
//
// OpenAI-style providers don't expose a separate cache_creation
// metric — they bill all prompt_tokens, and "cached" just means
// "served from prefix cache, charged at a discount". So we leave
// CacheCreationInputTokens at zero on this path.
type oaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
	CachedTokens         int `json:"cached_tokens,omitempty"`
}

// cacheReadTokens returns the cached-prefix token count, normalising
// across the three wire shapes (OpenAI nested, DeepSeek flat,
// Kimi flat). First non-zero wins. Returns 0 when none of the three
// upstreams reported a cache hit — that's also the correct value
// for "cold prompt, nothing cached yet" or "tiny prompt under the
// cache threshold (OpenAI requires ≥1024 tokens, DeepSeek ≥64)".
func cacheReadTokens(u oaiUsage) int {
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	if u.PromptCacheHitTokens > 0 {
		return u.PromptCacheHitTokens
	}
	if u.CachedTokens > 0 {
		return u.CachedTokens
	}
	return 0
}

// normalizeInputUsage converts an OpenAI-family total-input count into the
// provider-neutral disjoint usage buckets. OpenAI Chat Completions and
// Responses both include cached prefix tokens in prompt_tokens/input_tokens;
// exposing that total as InputTokens while also exposing cacheRead would count
// the cached prefix twice. Malformed upstream usage where cached exceeds total
// is clamped at zero uncached input rather than producing a negative bucket.
func normalizeInputUsage(total, cacheRead int) (inputTokens, cacheReadInputTokens int) {
	if total < 0 {
		total = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	// The wire contract says cached input is a subset of total input. Keep a
	// malformed compatibility gateway from manufacturing prompt occupancy by
	// reporting a cached count larger than its own total.
	if cacheRead > total {
		cacheRead = total
	}
	return max(total-cacheRead, 0), cacheRead
}

type oaiResp struct {
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

// --- conversion ---

// flattenSystem joins req.SystemSections (each with their own body)
// into a single string for the OpenAI-dialect system message. When
// SystemSections is empty falls back to req.System unchanged.
//
// Sections are joined with `\n\n` so block-level structure (like
// fenced <memory-context> or <auto-retrieve> tags) survives. Section
// names aren't injected as headers — the body text already carries
// any markers the model needs (e.g. "[System note: ...]" prepended
// inside AutoRetrieve / BuildContext).
func flattenSystem(req Request) string {
	if len(req.SystemSections) == 0 {
		return req.System
	}
	var sb strings.Builder
	for i, s := range req.SystemSections {
		if s.Body == "" {
			continue
		}
		if i > 0 && sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(s.Body)
	}
	return sb.String()
}

func toOpenAI(req Request, model string, maxTokens int) oaiReq {
	mt := maxTokens
	if req.MaxTokens > 0 {
		mt = req.MaxTokens
	}
	out := oaiReq{
		Model:           model,
		MaxTokens:       mt,
		Stream:          req.Stream,
		ReasoningEffort: req.Effort.OpenAI(),
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.Temperature = &t
	}
	if req.Stream {
		out.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	// System prompt: prefer SystemSections when populated (the path the
	// agent loop uses once it has memory + auto-retrieve to inject),
	// fall back to req.System otherwise.
	//
	// Pre-fix (2026-05-15): only req.System was read. The agent's
	// buildRequest writes memory / auto-retrieve into SystemSections
	// when they're non-empty, so DeepSeek / Kimi / MiniMax / GLM —
	// every OpenAI-dialect provider — silently dropped both
	// subsystems. AutoRetrieve appeared to work (debug log said
	// "enabled") but the model never saw the retrieved passages,
	// because the sections never reached the wire.
	//
	// Anthropic provider has dedicated section serialization with
	// per-section cache_control; OpenAI dialect has no equivalent so
	// we just join all section bodies into the single system message
	// with `\n\n` separators. Cache benefit is lost (DeepSeek's
	// prompt cache is prefix-only; volatile sections at the end will
	// invalidate downstream cache anyway), but at least the content
	// reaches the model.
	if sys := flattenSystem(req); sys != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			// User message can contain text + tool_results + images.
			// tool_results split off into role="tool" messages (OpenAI
			// dialect). text + images compose into the user message
			// itself — when images are present we emit a content-parts
			// array so OpenAI / DeepSeek / Kimi vision models can see
			// them; when there are NO images we keep the historical
			// string-content shape (cheaper to parse, every provider
			// accepts it).
			var text strings.Builder
			var toolMsgs []oaiMessage
			var images []oaiContentPart
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(c.Text)
				case "tool_result":
					// Vision-aware tools (ViewImage) populate
					// ToolResultBlocks with [{text}, {image}...]. OpenAI's
					// tool role only accepts a string Content — images
					// can't ride inside the tool message. So we siphon
					// any image blocks into the shared `images` slice
					// (which folds into the synthetic user message
					// below) and keep the textual portion as the tool
					// message body. The model then sees the tool reply
					// followed by a user message carrying the image,
					// preserving the visual context within the same
					// turn. Used by DeepSeek/Kimi/GLM (all openai-compat
					// custom providers) so ViewImage works there too,
					// not just on the Anthropic path.
					textPart := c.ToolResult
					if len(c.ToolResultBlocks) > 0 {
						var sb strings.Builder
						for _, b := range c.ToolResultBlocks {
							switch b.Type {
							case "text":
								if sb.Len() > 0 {
									sb.WriteByte('\n')
								}
								sb.WriteString(b.Text)
							case "image":
								images = append(images, oaiContentPart{
									Type: "image_url",
									ImageURL: &oaiImageURL{
										URL: "data:" + b.MediaType + ";base64," + b.Data,
									},
								})
							}
						}
						if sb.Len() > 0 {
							textPart = sb.String()
						}
					}
					toolMsgs = append(toolMsgs, oaiMessage{
						Role: "tool", ToolCallID: c.ToolUseID, Content: textPart,
					})
				case "image":
					images = append(images, oaiContentPart{
						Type: "image_url",
						ImageURL: &oaiImageURL{
							URL: "data:" + c.MediaType + ";base64," + c.Data,
						},
					})
				}
			}
			// IMPORTANT: tool messages must come BEFORE the next user text in OpenAI's
			// turn-taking model. We emit tool messages first, then any user text.
			out.Messages = append(out.Messages, toolMsgs...)
			if len(images) > 0 {
				parts := make([]oaiContentPart, 0, 1+len(images))
				if text.Len() > 0 {
					parts = append(parts, oaiContentPart{Type: "text", Text: text.String()})
				}
				parts = append(parts, images...)
				out.Messages = append(out.Messages, oaiMessage{Role: "user", Content: parts})
			} else if text.Len() > 0 {
				out.Messages = append(out.Messages, oaiMessage{Role: "user", Content: text.String()})
			}
		case RoleAssistant:
			am := oaiMessage{Role: "assistant"}
			var text strings.Builder
			var thinking strings.Builder
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(c.Text)
				case "tool_use":
					tc := oaiToolCall{ID: c.ToolUseID, Type: "function"}
					tc.Function.Name = c.ToolName
					tc.Function.Arguments = marshalToolArguments(c.ToolInput)
					am.ToolCalls = append(am.ToolCalls, tc)
				case "thinking":
					// Captured reasoning trace from a prior turn. Keep
					// it for the reasoning_content field below; do NOT
					// emit it as plain text or it'll show up in the
					// next response. DeepSeek/GLM ignore unknown
					// fields, so this is safe across providers.
					if thinking.Len() > 0 {
						thinking.WriteByte('\n')
					}
					thinking.WriteString(c.Text)
				}
			}
			am.Content = text.String()
			// Moonshot/Kimi thinking-tier models reject assistant
			// tool-call messages that lack reasoning_content. Either
			// pass through the actual reasoning we captured (preferred
			// — the model gets a faithful round-trip) or fall back to
			// an empty string (field is required-present, not
			// required-non-empty). Other OpenAI-compatible providers
			// ignore the field.
			if len(am.ToolCalls) > 0 || thinking.Len() > 0 {
				rc := thinking.String()
				am.ReasoningContent = &rc
			}
			out.Messages = append(out.Messages, am)
		}
	}
	for _, t := range req.Tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		out.Tools = append(out.Tools, ot)
	}
	return out
}

// marshalToolArguments preserves the OpenAI function-call invariant that
// `function.arguments` contains a JSON OBJECT string. Older Metis sessions can
// contain a tool_use without `input` (decoded as a nil map) when an upstream
// stream ended after the tool name but before its argument delta. json.Marshal
// on that value produces the syntactically-valid string "null"; several
// OpenAI-compatible chat templates (including SenseNova) then fail while
// iterating it as a mapping, poisoning every later turn in the session.
//
// Empty-object fallback is deliberately request-local: it repairs already
// persisted histories without rewriting the user's append-only transcript.
// The matching tool_result still tells the model that required fields were
// missing, so no failed action is reclassified as successful.
func marshalToolArguments(input map[string]any) string {
	if input == nil {
		return "{}"
	}
	b, err := json.Marshal(input)
	if err != nil || string(b) == "null" {
		return "{}"
	}
	return string(b)
}

func fromOpenAIChoice(c oaiChoice, usage oaiUsage) *Response {
	inputTokens, cacheReadInputTokens := normalizeInputUsage(
		usage.PromptTokens,
		cacheReadTokens(usage),
	)
	out := &Response{
		StopReason:           mapOAIStop(c.FinishReason),
		InputTokens:          inputTokens,
		OutputTokens:         usage.CompletionTokens,
		CacheReadInputTokens: cacheReadInputTokens,
	}
	// Reasoning is emitted before visible text/tool calls. Preserving that
	// chronology lets the shared stream consumer persist [thinking, text, tool]
	// in exactly the order the model produced it.
	if reasoning := c.Message.reasoningText(); reasoning != "" {
		out.Content = append(out.Content, ContentBlock{Type: "thinking", Text: reasoning})
	}
	// oaiMessage.Content is `any` (request side may be string OR
	// content-parts array for multimodal user turns). The response
	// side always carries a string, so assert and skip on mismatch.
	if str, ok := c.Message.Content.(string); ok && str != "" {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: str})
	}
	toolIDPrefix := ""
	for i, tc := range c.Message.ToolCalls {
		input := map[string]any{}
		if raw := strings.TrimSpace(tc.Function.Arguments); raw != "" && raw != "null" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed != nil {
				input = parsed
			} else if err != nil {
				// Match the streaming path: retain malformed bytes for a useful
				// tool-side error instead of collapsing the whole input to nil.
				input["_raw"] = tc.Function.Arguments
			}
		}
		id := tc.ID
		if id == "" {
			if toolIDPrefix == "" {
				toolIDPrefix = newSyntheticToolIDPrefix()
			}
			id = syntheticToolUseID(toolIDPrefix, i)
		}
		out.Content = append(out.Content, ContentBlock{
			Type: "tool_use", ToolUseID: id, ToolName: tc.Function.Name, ToolInput: input,
		})
	}
	return out
}

func mapOAIStop(s string) string {
	switch s {
	case "tool_calls":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return s
	}
}

// --- Provider impl ---

func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	if o.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set OPENAI_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	release, err := o.acquireRequestSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var or oaiResp
	// lastBody holds the most recent 4xx response body so the
	// post-loop overflow recovery can inspect it without a second
	// round-trip. Mirrors CC's withRetry.ts pattern where the retry
	// context carries the failing response forward.
	var lastBody string

	doOnce := func(maxTokens int) error {
		body := toOpenAI(req, o.Model, maxTokens)
		body.Stream = false
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return transport.RetryWithBackoff(ctx, 3, 0, func() error {
			lastBody = ""
			if err := o.waitRateCooldown(ctx); err != nil {
				return err
			}
			httpReq, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/chat/completions", bytes.NewReader(buf))
			if err != nil {
				return err
			}
			o.setHeaders(httpReq)
			resp, err := o.httpClient.Do(httpReq)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			rb, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				return o.responseBodyReadError(resp, readErr)
			}
			if resp.StatusCode >= 400 {
				lastBody = string(rb)
				httpErr := fmt.Errorf("openai %d: %s", resp.StatusCode, transport.Truncate(lastBody, 500))
				if transport.IsRetryableStatus(resp.StatusCode) {
					after := transport.ParseRetryAfter(resp)
					if resp.StatusCode == http.StatusTooManyRequests {
						after = o.noteRateLimit(after)
					}
					return &transport.RetryableError{Err: httpErr, After: after}
				}
				return httpErr
			}
			o.noteRateSuccess()
			return json.Unmarshal(rb, &or)
		})
	}

	err = doOnce(o.MaxTokens)
	if err != nil {
		// CC-aligned auto-recovery for 400 context overflow: parse the
		// input/cap figures out of the failing body, reduce max_tokens
		// to whatever room remains, and retry once. If even the floor
		// completion budget won't fit, wrap the error with the Fork→Agent
		// hint so the model knows to cold-spawn instead.
		if in, cap, ok := transport.ParseContextOverflow(lastBody); ok {
			if adjusted, retryOK := transport.ComputeAdjustedMaxTokens(in, cap); retryOK {
				if dbgOpenAI {
					fmt.Fprintf(os.Stderr, "[openai] context overflow %d/%d — retrying with max_tokens=%d\n", in, cap, adjusted)
				}
				lastBody = ""
				err = doOnce(adjusted)
			} else {
				err = fmt.Errorf("%w\n\n%s", err, transport.BuildOverflowHint(in, cap))
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if len(or.Choices) == 0 {
		return nil, errors.New("openai: empty choices")
	}
	return fromOpenAIChoice(or.Choices[0], or.Usage), nil
}

func (o *OpenAI) Stream(ctx context.Context, req Request) (StreamReader, error) {
	if o.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set OPENAI_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	release, err := o.acquireRequestSlot(ctx)
	if err != nil {
		return nil, err
	}

	var resp *http.Response
	var lastBody string

	openOnce := func(maxTokens int) error {
		body := toOpenAI(req, o.Model, maxTokens)
		body.Stream = true
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return transport.RetryWithBackoff(ctx, 3, 0, func() error {
			lastBody = ""
			if err := o.waitRateCooldown(ctx); err != nil {
				return err
			}
			httpReq, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/chat/completions", bytes.NewReader(buf))
			if err != nil {
				return err
			}
			o.setHeaders(httpReq)
			resp, err = o.httpClient.Do(httpReq)
			if err != nil {
				return err
			}
			if resp.StatusCode >= 400 {
				rb, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return o.responseBodyReadError(resp, readErr)
				}
				lastBody = string(rb)
				httpErr := fmt.Errorf("openai %d: %s", resp.StatusCode, transport.Truncate(lastBody, 500))
				if transport.IsRetryableStatus(resp.StatusCode) {
					after := transport.ParseRetryAfter(resp)
					if resp.StatusCode == http.StatusTooManyRequests {
						after = o.noteRateLimit(after)
					}
					return &transport.RetryableError{Err: httpErr, After: after}
				}
				return httpErr
			}
			o.noteRateSuccess()
			return nil
		})
	}

	err = openOnce(o.MaxTokens)
	if err != nil {
		if in, cap, ok := transport.ParseContextOverflow(lastBody); ok {
			if adjusted, retryOK := transport.ComputeAdjustedMaxTokens(in, cap); retryOK {
				if dbgOpenAI {
					fmt.Fprintf(os.Stderr, "[openai stream] context overflow %d/%d — retrying with max_tokens=%d\n", in, cap, adjusted)
				}
				lastBody = ""
				err = openOnce(adjusted)
			} else {
				err = fmt.Errorf("%w\n\n%s", err, transport.BuildOverflowHint(in, cap))
			}
		}
	}
	if err != nil {
		release()
		return nil, err
	}
	return &requestSlotStream{StreamReader: newOpenAIStream(resp.Body), release: release}, nil
}

// requestSlotStream retains an OpenAI concurrency slot for the lifetime of a
// streaming generation, not merely until response headers arrive. Recv errors
// and Close both release idempotently so early cancellation cannot leak slots.
type requestSlotStream struct {
	StreamReader
	release func()
	once    sync.Once
}

func (s *requestSlotStream) releaseOnce() {
	s.once.Do(s.release)
}

func (s *requestSlotStream) Recv() (StreamEvent, error) {
	ev, err := s.StreamReader.Recv()
	if err != nil {
		s.releaseOnce()
	}
	return ev, err
}

func (s *requestSlotStream) Close() error {
	err := s.StreamReader.Close()
	s.releaseOnce()
	return err
}

func (o *OpenAI) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+o.APIKey)
}

// responseBodyReadError preserves both transport and HTTP retry semantics.
// A truncated 200 is retryable through the wrapped io error; a truncated
// 429/5xx additionally retains Retry-After and the shared 429 cooldown even
// though its error JSON could not be read completely.
func (o *OpenAI) responseBodyReadError(resp *http.Response, readErr error) error {
	err := fmt.Errorf("openai %d response body: %w", resp.StatusCode, readErr)
	if !transport.IsRetryableStatus(resp.StatusCode) {
		return err
	}
	after := transport.ParseRetryAfter(resp)
	if resp.StatusCode == http.StatusTooManyRequests {
		after = o.noteRateLimit(after)
	}
	return &transport.RetryableError{Err: err, After: after}
}

// --- SSE stream ---

type openAIStream struct {
	body io.ReadCloser
	sse  *sse.Reader
	// toolIDPrefix is random for every response. Unlike a process-local
	// counter, it cannot repeat merely because Metis restarted while old
	// synthetic ids are still present in session history.
	toolIDPrefix string
	// Calls are keyed by a response-local identity rather than by the wire
	// index. OpenAI-compatible gateways are inconsistent about which delta
	// carries id/index, so both fields bind to this stable internal key.
	calls        map[int]*oaiCallAccum
	callOrder    []int
	emittedStart map[int]bool
	idToKey      map[string]int
	indexToKey   map[int]int
	keyToIndex   map[int]int
	nextCallKey  int
	// unindexedOrder records calls first introduced without a wire index.
	// If a later delta supplies only the index, its ordinal lets us reconcile
	// that delta with the already-started call instead of splitting it.
	unindexedOrder []int
	// anonymousOrder provides the only sound fallback available when a
	// non-standard provider omits both id and index. A sole name-only start
	// followed by argument-only chunks remains one logical call; parallel
	// anonymous deltas are associated by their array ordinal.
	anonymousOrder []int
	// Some providers (Gemini's OpenAI-compat layer) bundle multiple logical
	// events into a single SSE chunk (content + finish_reason + usage).
	// We queue events so each Recv() returns exactly one.
	pending []StreamEvent
}

type oaiCallAccum struct {
	ID          string
	SyntheticID bool
	Name        string
	JSONBuf     strings.Builder
}

func newOpenAIStream(body io.ReadCloser) *openAIStream {
	return &openAIStream{
		body:         body,
		sse:          sse.NewReader(body),
		toolIDPrefix: newSyntheticToolIDPrefix(),
		calls:        make(map[int]*oaiCallAccum),
		emittedStart: make(map[int]bool),
		idToKey:      make(map[string]int),
		indexToKey:   make(map[int]int),
		keyToIndex:   make(map[int]int),
	}
}

func newSyntheticToolIDPrefix() string {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err == nil {
		return hex.EncodeToString(nonce[:])
	}
	// crypto/rand failures are exceptional, but tool parsing must not fail
	// merely because the OS entropy source is temporarily unavailable. The
	// time/process fallback is still response-scoped and does not reset to a
	// small global counter after a restart.
	fallback := []byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid()))
	return hex.EncodeToString(fallback)
}

func syntheticToolUseID(prefix string, key int) string {
	return fmt.Sprintf("metis_oai_%s_%d", prefix, key)
}

func (s *openAIStream) Close() error { return s.body.Close() }

func (s *openAIStream) newCallKey(hasIndex, anonymous bool) int {
	key := s.nextCallKey
	s.nextCallKey++
	if !hasIndex {
		s.unindexedOrder = append(s.unindexedOrder, key)
	}
	if anonymous {
		s.anonymousOrder = append(s.anonymousOrder, key)
	}
	return key
}

func (s *openAIStream) bindIndex(idx, key int) {
	s.indexToKey[idx] = key
	s.keyToIndex[key] = idx
}

func (s *openAIStream) firstUnindexedKey(idx int) (int, bool) {
	// Wire index is the call's ordinal among all calls, including calls whose
	// earlier frames already carried an index. Use creation order rather than
	// indexing into only the unbound subset.
	if idx >= 0 && idx < len(s.callOrder) {
		key := s.callOrder[idx]
		if _, bound := s.keyToIndex[key]; !bound {
			return key, true
		}
	}
	// A non-zero/irregular provider index cannot be matched by ordinal. It is
	// still safe to reconcile when exactly one call remains unbound.
	unboundKey := 0
	unboundCount := 0
	for _, key := range s.unindexedOrder {
		if _, bound := s.keyToIndex[key]; !bound {
			unboundKey = key
			unboundCount++
		}
	}
	return unboundKey, unboundCount == 1
}

func (s *openAIStream) resolveAnonymousKey(tc oaiToolCall, ordinal int) int {
	// A function name marks the start of a new anonymous call. Subsequent
	// argument-only frames reuse it. If several anonymous calls are carried
	// in parallel arrays, their array ordinal is the best identity available.
	if tc.Function.Name != "" {
		return s.newCallKey(false, true)
	}
	// Some gateways send id only on the name frame, then omit both id and
	// index from arguments. The sole active call is unambiguous regardless of
	// whether it began as anonymous, id-only, or index-only.
	if len(s.calls) == 1 {
		for key := range s.calls {
			return key
		}
	}
	if ordinal >= 0 && ordinal < len(s.callOrder) {
		key := s.callOrder[ordinal]
		if _, active := s.calls[key]; active {
			return key
		}
	}
	if len(s.anonymousOrder) > 0 {
		return s.anonymousOrder[len(s.anonymousOrder)-1]
	}
	return s.newCallKey(false, true)
}

// resolveToolCallKey assigns a stable response-local identity to a streamed
// tool_call delta. ID is authoritative when it has been seen before, even if
// a later frame introduces an index. This prevents an id-only start and an
// id+index argument frame from being split into separate accumulators.
func (s *openAIStream) resolveToolCallKey(tc oaiToolCall, ordinal int) int {
	if tc.ID != "" {
		if key, ok := s.idToKey[tc.ID]; ok {
			if tc.Index != nil {
				s.bindIndex(*tc.Index, key)
			}
			return key
		}
	}

	if tc.Index != nil {
		idx := *tc.Index
		if key, ok := s.indexToKey[idx]; ok {
			if tc.ID != "" {
				s.idToKey[tc.ID] = key
			}
			return key
		}
		// With no id, a newly introduced index can only be reconciled by
		// response order. A new, non-empty id is authoritative evidence of a
		// distinct call and must never be folded into an unrelated id-only call.
		if tc.ID == "" {
			if key, ok := s.firstUnindexedKey(idx); ok {
				s.bindIndex(idx, key)
				return key
			}
		}
		key := s.newCallKey(true, false)
		s.bindIndex(idx, key)
		if tc.ID != "" {
			s.idToKey[tc.ID] = key
		}
		return key
	}

	if tc.ID != "" {
		key := s.newCallKey(false, false)
		s.idToKey[tc.ID] = key
		return key
	}

	return s.resolveAnonymousKey(tc, ordinal)
}

// flushToolStops emits tool stops in the same stable order in which their
// accumulators were created. Ranging over calls directly made parallel tool
// blocks nondeterministic because Go deliberately randomizes map iteration.
func (s *openAIStream) flushToolStops() {
	for _, idx := range s.callOrder {
		c, ok := s.calls[idx]
		if !ok {
			continue
		}
		if s.emittedStart[idx] {
			s.pending = append(s.pending, StreamEvent{Type: "tool_use_stop", ToolUseID: c.ID, InputDelta: c.JSONBuf.String()})
		}
		delete(s.calls, idx)
		delete(s.emittedStart, idx)
	}
	s.callOrder = s.callOrder[:0]
}

func (s *openAIStream) Recv() (StreamEvent, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		// io.EOF still needs to be propagated as the final return.
		if ev.Type == "message_stop" && len(s.pending) == 0 {
			return ev, io.EOF
		}
		return ev, nil
	}
	for {
		frame, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return StreamEvent{Type: "message_stop"}, io.EOF
			}
			return StreamEvent{Type: "error", Err: err}, err
		}
		payload := frame.Data
		if dbgOpenAI {
			fmt.Fprintf(os.Stderr, "[oai] raw=%s\n", payload)
		}
		if payload == "[DONE]" {
			// flush any pending tool_use_stop, then enqueue message_stop.
			s.flushToolStops()
			s.pending = append(s.pending, StreamEvent{Type: "message_stop"})
			return s.popPending()
		}

		var env struct {
			Choices []struct {
				Delta        oaiMessage `json:"delta"`
				FinishReason string     `json:"finish_reason"`
			} `json:"choices"`
			Usage *oaiUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			continue
		}

		// Process choice deltas (content / tool_calls / finish_reason).
		// Each chunk may contain multiple of these — enqueue all, return first.
		if len(env.Choices) > 0 {
			ch := env.Choices[0]
			// DeepSeek/Ark/Kimi/vLLM reasoning arrives beside ordinary content
			// in the delta object. Emit it first when a gateway bundles both in
			// one SSE frame so consumeStream flushes a chronological thinking
			// block before the visible answer or tool call.
			if reasoning := ch.Delta.reasoningText(); reasoning != "" {
				s.pending = append(s.pending, StreamEvent{Type: "thinking_delta", TextDelta: reasoning})
			}
			// Stream delta content is always a string in OpenAI SSE
			// (assistants never stream images). Type-assert to string;
			// ignore non-string values (defensive — a vendor extension
			// could ship structured deltas).
			if str, ok := ch.Delta.Content.(string); ok && str != "" {
				s.pending = append(s.pending, StreamEvent{Type: "text_delta", TextDelta: str})
			}
			for ordinal, tc := range ch.Delta.ToolCalls {
				key := s.resolveToolCallKey(tc, ordinal)
				c, ok := s.calls[key]
				if !ok {
					id := tc.ID
					synthetic := false
					if id == "" {
						id = syntheticToolUseID(s.toolIDPrefix, key)
						synthetic = true
					}
					c = &oaiCallAccum{ID: id, SyntheticID: synthetic}
					s.calls[key] = c
					s.callOrder = append(s.callOrder, key)
				}
				// If the provider sends the real id in a later pre-start chunk,
				// prefer it. Once tool_use_start has been emitted the event id is
				// immutable: changing it would make later deltas/stops impossible
				// for consumeStream to associate with the reserved content block.
				if tc.ID != "" && !s.emittedStart[key] {
					c.ID = tc.ID
					c.SyntheticID = false
				}
				if tc.Function.Name != "" {
					c.Name = tc.Function.Name
					if !s.emittedStart[key] {
						s.emittedStart[key] = true
						s.pending = append(s.pending, StreamEvent{Type: "tool_use_start", ToolUseID: c.ID, ToolName: c.Name})
					}
				}
				if tc.Function.Arguments != "" {
					c.JSONBuf.WriteString(tc.Function.Arguments)
					s.pending = append(s.pending, StreamEvent{Type: "tool_input_delta", ToolUseID: c.ID, InputDelta: tc.Function.Arguments})
				}
			}
			if ch.FinishReason != "" {
				s.flushToolStops()
				ev := StreamEvent{Type: "message_delta", StopReason: mapOAIStop(ch.FinishReason)}
				if env.Usage != nil {
					ev.InputTokens, ev.CacheReadInputTokens = normalizeInputUsage(
						env.Usage.PromptTokens,
						cacheReadTokens(*env.Usage),
					)
					ev.OutputTokens = env.Usage.CompletionTokens
				}
				s.pending = append(s.pending, ev)
			} else if env.Usage != nil {
				inputTokens, cacheReadInputTokens := normalizeInputUsage(
					env.Usage.PromptTokens,
					cacheReadTokens(*env.Usage),
				)
				s.pending = append(s.pending, StreamEvent{
					Type:                 "message_delta",
					InputTokens:          inputTokens,
					OutputTokens:         env.Usage.CompletionTokens,
					CacheReadInputTokens: cacheReadInputTokens,
				})
			}
		} else if env.Usage != nil {
			inputTokens, cacheReadInputTokens := normalizeInputUsage(
				env.Usage.PromptTokens,
				cacheReadTokens(*env.Usage),
			)
			s.pending = append(s.pending, StreamEvent{
				Type:                 "message_delta",
				InputTokens:          inputTokens,
				OutputTokens:         env.Usage.CompletionTokens,
				CacheReadInputTokens: cacheReadInputTokens,
			})
		}

		if len(s.pending) > 0 {
			return s.popPending()
		}
		// Frame had no usable content (e.g. heartbeat) — keep reading.
	}
}

func (s *openAIStream) popPending() (StreamEvent, error) {
	ev := s.pending[0]
	s.pending = s.pending[1:]
	if ev.Type == "message_stop" && len(s.pending) == 0 {
		return ev, io.EOF
	}
	return ev, nil
}
