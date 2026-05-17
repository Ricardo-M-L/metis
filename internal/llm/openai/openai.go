// Package openai implements the OpenAI Chat Completions API
// (api.openai.com/v1/chat/completions and OpenAI-compatible gateways:
// DeepSeek, Together, Groq, Cerebras, MiniMax /v1, Ollama …). Used
// directly when transport=openai_chat, and consumed by the azure
// subpackage which runs the same wire format with deployment-routed URLs.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
		APIKey:      apiKey,
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		httpClient:  transport.NewHTTPClient(timeout, "openai"),
	}
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
	// Future: extend the Anthropic + OpenAI stream parsers to capture
	// thinking_delta / reasoning_content_delta as a "thinking"
	// ContentBlock type, then this field would round-trip the model's
	// actual chain of thought instead of an empty string. Tracked.
	ReasoningContent *string `json:"reasoning_content,omitempty"`
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
					toolMsgs = append(toolMsgs, oaiMessage{
						Role: "tool", ToolCallID: c.ToolUseID, Content: c.ToolResult,
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
					argsJSON, _ := json.Marshal(c.ToolInput)
					tc := oaiToolCall{ID: c.ToolUseID, Type: "function"}
					tc.Function.Name = c.ToolName
					tc.Function.Arguments = string(argsJSON)
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

func fromOpenAIChoice(c oaiChoice, usage oaiUsage) *Response {
	out := &Response{
		StopReason:           mapOAIStop(c.FinishReason),
		InputTokens:          usage.PromptTokens,
		OutputTokens:         usage.CompletionTokens,
		CacheReadInputTokens: cacheReadTokens(usage),
	}
	// oaiMessage.Content is `any` (request side may be string OR
	// content-parts array for multimodal user turns). The response
	// side always carries a string, so assert and skip on mismatch.
	if str, ok := c.Message.Content.(string); ok && str != "" {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: str})
	}
	for _, tc := range c.Message.ToolCalls {
		var input map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		out.Content = append(out.Content, ContentBlock{
			Type: "tool_use", ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: input,
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
			rb, _ := io.ReadAll(resp.Body)
			if resp.StatusCode >= 400 {
				lastBody = string(rb)
				httpErr := fmt.Errorf("openai %d: %s", resp.StatusCode, transport.Truncate(lastBody, 500))
				if transport.IsRetryableStatus(resp.StatusCode) {
					return &transport.RetryableError{Err: httpErr}
				}
				return httpErr
			}
			return json.Unmarshal(rb, &or)
		})
	}

	err := doOnce(o.MaxTokens)
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
				rb, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				lastBody = string(rb)
				httpErr := fmt.Errorf("openai %d: %s", resp.StatusCode, transport.Truncate(lastBody, 500))
				if transport.IsRetryableStatus(resp.StatusCode) {
					return &transport.RetryableError{Err: httpErr}
				}
				return httpErr
			}
			return nil
		})
	}

	err := openOnce(o.MaxTokens)
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
		return nil, err
	}
	return newOpenAIStream(resp.Body), nil
}

func (o *OpenAI) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+o.APIKey)
}

// --- SSE stream ---

type openAIStream struct {
	body io.ReadCloser
	sse  *sse.Reader
	// per-tool-call accumulator keyed by index
	calls        map[int]*oaiCallAccum
	emittedStart map[int]bool
	// idToIdx maps a tool_call.id to the synthetic index we picked when the
	// upstream provider didn't send one. Some OpenAI-compat servers (Groq,
	// Together) omit the index field on parallel tool calls and the previous
	// "default to 0" code collapsed every call into calls[0].
	idToIdx          map[string]int
	nextSyntheticIdx int
	// Some providers (Gemini's OpenAI-compat layer) bundle multiple logical
	// events into a single SSE chunk (content + finish_reason + usage).
	// We queue events so each Recv() returns exactly one.
	pending []StreamEvent
}

type oaiCallAccum struct {
	ID      string
	Name    string
	JSONBuf strings.Builder
}

func newOpenAIStream(body io.ReadCloser) *openAIStream {
	return &openAIStream{
		body:         body,
		sse:          sse.NewReader(body),
		calls:        make(map[int]*oaiCallAccum),
		emittedStart: make(map[int]bool),
		idToIdx:      make(map[string]int),
	}
}

func (s *openAIStream) Close() error { return s.body.Close() }

// resolveToolCallIdx assigns a stable map key for a streamed tool_call delta.
// The OpenAI spec sends `index` for every chunk, but Groq/Together and other
// compat servers sometimes omit it on parallel calls — we then fall back to
// the call id, and if even that is missing we hand out fresh synthetic
// indices. The previous code defaulted to 0 here, which collapsed every
// indexless tool call into a single accumulator.
func (s *openAIStream) resolveToolCallIdx(tc oaiToolCall) int {
	if tc.Index != nil {
		return *tc.Index
	}
	if tc.ID != "" {
		if known, ok := s.idToIdx[tc.ID]; ok {
			return known
		}
		// Pick a synthetic slot above any real index we've seen so we
		// don't collide with future numbered chunks.
		idx := s.nextSyntheticIdx
		s.nextSyntheticIdx++
		s.idToIdx[tc.ID] = idx
		return idx
	}
	idx := s.nextSyntheticIdx
	s.nextSyntheticIdx++
	return idx
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
			for idx, c := range s.calls {
				if s.emittedStart[idx] {
					s.pending = append(s.pending, StreamEvent{Type: "tool_use_stop", ToolUseID: c.ID, InputDelta: c.JSONBuf.String()})
				}
				delete(s.calls, idx)
				delete(s.emittedStart, idx)
			}
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
			// Stream delta content is always a string in OpenAI SSE
			// (assistants never stream images). Type-assert to string;
			// ignore non-string values (defensive — a vendor extension
			// could ship structured deltas).
			if str, ok := ch.Delta.Content.(string); ok && str != "" {
				s.pending = append(s.pending, StreamEvent{Type: "text_delta", TextDelta: str})
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := s.resolveToolCallIdx(tc)
				c, ok := s.calls[idx]
				if !ok {
					c = &oaiCallAccum{}
					s.calls[idx] = c
				}
				if tc.ID != "" {
					c.ID = tc.ID
				}
				if tc.Function.Name != "" {
					c.Name = tc.Function.Name
					if !s.emittedStart[idx] {
						s.emittedStart[idx] = true
						s.pending = append(s.pending, StreamEvent{Type: "tool_use_start", ToolUseID: c.ID, ToolName: c.Name})
					}
				}
				if tc.Function.Arguments != "" {
					c.JSONBuf.WriteString(tc.Function.Arguments)
					s.pending = append(s.pending, StreamEvent{Type: "tool_input_delta", ToolUseID: c.ID, InputDelta: tc.Function.Arguments})
				}
			}
			if ch.FinishReason != "" {
				for idx, c := range s.calls {
					if s.emittedStart[idx] {
						s.pending = append(s.pending, StreamEvent{Type: "tool_use_stop", ToolUseID: c.ID, InputDelta: c.JSONBuf.String()})
					}
					delete(s.calls, idx)
					delete(s.emittedStart, idx)
				}
				ev := StreamEvent{Type: "message_delta", StopReason: mapOAIStop(ch.FinishReason)}
				if env.Usage != nil {
					ev.InputTokens = env.Usage.PromptTokens
					ev.OutputTokens = env.Usage.CompletionTokens
					ev.CacheReadInputTokens = cacheReadTokens(*env.Usage)
				}
				s.pending = append(s.pending, ev)
			} else if env.Usage != nil {
				s.pending = append(s.pending, StreamEvent{
					Type:                 "message_delta",
					InputTokens:          env.Usage.PromptTokens,
					OutputTokens:         env.Usage.CompletionTokens,
					CacheReadInputTokens: cacheReadTokens(*env.Usage),
				})
			}
		} else if env.Usage != nil {
			s.pending = append(s.pending, StreamEvent{
				Type:                 "message_delta",
				InputTokens:          env.Usage.PromptTokens,
				OutputTokens:         env.Usage.CompletionTokens,
				CacheReadInputTokens: cacheReadTokens(*env.Usage),
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
