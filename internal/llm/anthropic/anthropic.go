// Package anthropic implements the Anthropic Messages API transport
// (api.anthropic.com/v1/messages and Anthropic-format gateways like
// MiniMax / OpenRouter / yunwu). Used directly when configured as
// transport=anthropic_messages, and consumed by the vertex/bedrock
// subpackages which run the same wire format over different
// transports.
package anthropic

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

	"github.com/Ricardo-M-L/metis/internal/llm/sse"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	pubLLM "github.com/Ricardo-M-L/metis/pkg/llm"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// Local type aliases keep the in-file diff small — the bulk of this
// file uses bare `Request`/`Response`/etc. and would be noisy if every
// reference grew a `provider.` qualifier.
type (
	Request      = provider.Request
	Response     = provider.Response
	StreamReader = provider.StreamReader
	StreamEvent  = provider.StreamEvent
	Message      = provider.Message
	ContentBlock = provider.ContentBlock
	ToolSpec     = provider.ToolSpec
	Effort       = pubLLM.Effort
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

// Anthropic implements Provider against the Messages API.
// We hand-roll the HTTP layer to keep the dependency footprint minimal
// and to control SSE parsing precisely.
type Anthropic struct {
	APIKey    string
	BaseURL   string
	Model     string
	MaxTokens int
	Beta      string
	// ContextWindow, when > 0, overrides the model-prefix lookup in
	// MaxContextTokens(). Lets users on Anthropic-compatible third-party
	// gateways (MiniMax, OpenRouter, ...) declare the real window so
	// auto-compaction triggers at the right threshold.
	ContextWindow int
	httpClient    *http.Client

	// AntiDistillation, when true, adds the top-level opt-in field
	// `anti_distillation: ["fake_tools"]` to every outgoing request.
	// Exact wire-format parity with claude-code's
	// services/api/claude.ts:312 — server-side opt-in, NOT a
	// client-side decoy injection.
	//
	// Only effective when BaseURL points at the real Anthropic API.
	// Third-party gateways (yunwu.ai, MiniMax, OpenRouter, Together,
	// Groq, ...) ignore the unknown field. Off by default; the
	// runtime emits a stderr warning if the flag is set against a
	// non-Anthropic BaseURL so users don't get a false sense of
	// protection.
	AntiDistillation bool

	// ClientSideDecoys, when true, attaches `_decoy_tools_v2_archive`
	// (a non-standard top-level field) to every outgoing request.
	// Field carries plausible-looking fake tool definitions; well-
	// behaved API consumers ignore unknown fields, so the model
	// never sees them — but anyone recording the HTTP traffic to
	// train a competing model gets the decoys mixed into their
	// dataset. Independent of AntiDistillation; both can be on.
	ClientSideDecoys bool
}

func New(apiKey, baseURL, model string, maxTokens int, timeout time.Duration, beta string) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if model == "" {
		model = "claude-opus-4-7"
	}
	if maxTokens == 0 {
		maxTokens = 8192
	}
	return &Anthropic{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Model:      model,
		MaxTokens:  maxTokens,
		Beta:       beta,
		httpClient: transport.NewHTTPClient(timeout, "anthropic"),
	}
}

// MaxContextTokens returns the effective context window for compaction.
// Precedence: explicit ContextWindow override > model-prefix lookup.
//
// For MiniMax-M* (Anthropic-compatible gateway) we default to 192k —
// MiniMax M2 publishes a 192k window, but their API's effective request
// budget is lower, so a too-large default lets requests overflow before
// compaction fires (the user hit "context window exceeds limit (2013)"
// at runtime). Set [provider.anthropic].context_window in config.toml
// if your gateway has a tighter cap.
func (a *Anthropic) MaxContextTokens() int {
	if a.ContextWindow > 0 {
		return a.ContextWindow
	}
	switch {
	case strings.HasPrefix(a.Model, "claude-opus"):
		return 200000
	case strings.HasPrefix(a.Model, "claude-sonnet"):
		return 200000
	case strings.HasPrefix(a.Model, "claude-haiku"):
		return 200000
	case strings.HasPrefix(a.Model, "MiniMax"), strings.HasPrefix(a.Model, "minimax"):
		return 192000
	default:
		return 200000 // safe default for unknown Anthropic-compatible models
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

// --- request shapes (Anthropic-native) ---

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        map[string]any         `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      any                    `json:"content,omitempty"` // string or []block in tool_result
	IsError      bool                   `json:"is_error,omitempty"`
	Source       *anthropicImageSource  `json:"source,omitempty"` // type="image" only
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicImageSource is the inline-base64 image carrier. Anthropic
// also supports `{"type":"url", "url":...}` but metis only ships
// pasted-clipboard bytes today, so we always emit the base64 form.
type anthropicImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png" / "image/jpeg" / "image/gif" / "image/webp"
	Data      string `json:"data"`
}

// MarshalJSON ensures `tool_use` blocks ALWAYS emit an `input` field,
// falling back to `{}` when the model produced no parameters.
//
// Why: real Anthropic accepts a missing `input` field on round-trip,
// but MiniMax's anthropic-compat endpoint and several other shims
// strictly require `input` to be present — they 400 with code (2013)
// `invalid function arguments json string` on the NEXT request that
// echoes the offending tool_use back. The default `omitempty` on
// `map[string]any{}` causes Go to drop the field entirely, so even
// after the user's retry-with-reminder shifts the model off the
// empty-args path, the previous turn's empty tool_use is still in
// the messages array and keeps tripping the 2013 reject.
//
// This MarshalJSON intercepts the tool_use case and emits a literal
// `{}` so subsequent requests round-trip cleanly. Other content types
// (text / tool_result) are passed through to the default marshaler.
func (c anthropicContent) MarshalJSON() ([]byte, error) {
	if c.Type != "tool_use" {
		type alias anthropicContent
		return json.Marshal(alias(c))
	}
	// Tool-use shape with mandatory (non-omitempty) input. We
	// reconstruct only the fields a tool_use block actually carries —
	// text / tool_use_id / content / is_error don't apply.
	type toolUseShape struct {
		Type         string                 `json:"type"`
		ID           string                 `json:"id,omitempty"`
		Name         string                 `json:"name,omitempty"`
		Input        map[string]any         `json:"input"` // NO omitempty
		CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
	}
	in := c.Input
	if in == nil {
		in = map[string]any{}
	}
	return json.Marshal(toolUseShape{
		Type: c.Type, ID: c.ID, Name: c.Name, Input: in, CacheControl: c.CacheControl,
	})
}

// anthropicCacheControl marks a tool / system block as a prompt-cache
// breakpoint. Anthropic spec: "ephemeral" gives ~5min TTL; everything
// BEFORE this marker (including the marker itself) is cached together.
// Up to 4 cache_control markers per request. A request hitting a cached
// prefix bills cache_read_input_tokens at ~10% of normal input cost.
type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	InputSchema  map[string]any         `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicSystemBlock is the array-form system prompt. Each block can
// carry its own cache_control, which lets us cache the static
// description text while keeping per-turn dynamic facts (cwd, date)
// outside the cache so a `cd otherproj` between calls still gets the
// correct env block.
type anthropicSystemBlock struct {
	Type         string                 `json:"type"` // always "text"
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicThinking is Anthropic's extended-thinking config block.
// We only ever send Type="enabled" with a positive BudgetTokens; nil = omit.
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicReq struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is sent as []anthropicSystemBlock when prompt caching is
	// active so the static prefix can carry cache_control. The Anthropic
	// API accepts either string or array; we always emit array form
	// (single block when no cache split needed) — gateways that don't
	// understand prompt caching just see plain text in block[0].Text.
	System        []anthropicSystemBlock `json:"system,omitempty"`
	Messages      []anthropicMessage     `json:"messages"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	Thinking      *anthropicThinking     `json:"thinking,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	// AntiDistillation: top-level opt-in field matching claude-code's
	// services/api/claude.ts:312 — `result.anti_distillation = ['fake_tools']`.
	// This is a SERVER-SIDE opt-in: the Anthropic backend reads the
	// flag and injects countermeasures into the response stream
	// itself. The client does NOT inject anything into tools[] — that
	// would just give the model fake tools to (mis)call, which is the
	// opposite of what we want.
	//
	// Only meaningful against the real Anthropic endpoint; gateways
	// like yunwu.ai / OpenRouter / MiniMax will silently ignore the
	// field. CC additionally gates this behind
	// `CLAUDE_CODE_ENTRYPOINT === "cli"` + Statsig — i.e. only the 1P
	// CLI emits it. metis exposes it as an opt-in toml flag.
	AntiDistillation []string `json:"anti_distillation,omitempty"`

	// DecoyToolsArchive is the client-side anti-distillation channel
	// (independent of the CC server-side opt-in above). Non-standard
	// JSON tag chosen so the field is dropped by every conforming
	// API consumer — including the model that ultimately renders the
	// prompt. Adversaries recording the HTTP traffic see the decoys
	// in the bytes; the model never does.
	//
	// Wire shape mirrors `tools[]` so a naive grep / regex training
	// pipeline picks up the noise. The intentional duplication of
	// `name`/`description`/`input_schema` keys is what makes the
	// channel useful; a stricter "structured-extraction" pipeline
	// might still filter it, but raw-token / next-token-prediction
	// training pipelines will absorb the decoys as real signal.
	DecoyToolsArchive []anthropicTool `json:"_decoy_tools_v2_archive,omitempty"`
}

type anthropicResp struct {
	ID         string             `json:"id"`
	Role       string             `json:"role"`
	Type       string             `json:"type"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Model      string             `json:"model"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// --- conversion helpers ---

func toAnthropic(req Request, model string, maxTokens int) anthropicReq {
	return toAnthropicWithFlags(req, model, maxTokens, false, false)
}

// toAnthropicWithFlags is the full-arity converter used by the Anthropic
// client. The flag-less wrapper above keeps existing callers / tests
// untouched.
//   - antiDistill toggles the CC server-side opt-in (top-level
//     `anti_distillation: ["fake_tools"]`).
//   - clientDecoys toggles the metis client-side decoy archive
//     (top-level `_decoy_tools_v2_archive` with fake tool defs).
//
// The two flags are independent — both can be on, both can be off.
func toAnthropicWithFlags(req Request, model string, maxTokens int, antiDistill, clientDecoys bool) anthropicReq {
	// MaxTokens precedence: per-request override > provider default.
	// The override path is what /fast uses to halve budget without
	// touching the underlying client config.
	mt := maxTokens
	if req.MaxTokens > 0 {
		mt = req.MaxTokens
	}
	out := anthropicReq{
		Model:         model,
		MaxTokens:     mt,
		System:        chooseSystemBlocks(req),
		Stream:        req.Stream,
		StopSequences: req.StopSequences,
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.Temperature = &t
	}
	// Extended thinking: only set when caller explicitly asked for an
	// effort level. Some Anthropic models (e.g. older Sonnet) reject
	// the field entirely, so silence-by-default is safer than an
	// always-on small budget.
	if budget := req.Effort.BudgetTokens(); budget > 0 {
		out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
		})
	}
	// Anti-distillation: send the ["fake_tools"] opt-in as a top-level
	// request field — exact wire-format parity with claude-code's
	// services/api/claude.ts:312. The Anthropic backend interprets
	// the flag and applies its own countermeasures; the client side
	// does NOT touch the tools[] array. Non-Anthropic gateways will
	// silently ignore the field (omitempty drops it when false).
	if antiDistill {
		out.AntiDistillation = []string{"fake_tools"}
	}
	// Client-side decoys: stuff the wire-only `_decoy_tools_v2_archive`
	// field with fake tool defs. The model never sees this — it's a
	// non-standard top-level field that every API silently drops
	// before constructing the prompt. But anyone capturing HTTP
	// traffic to train a competing model gets the decoys in their
	// dataset. See anthropic_client_decoys.go for the shape.
	if clientDecoys {
		out.DecoyToolsArchive = clientSideDecoyTools()
	}
	// Mark the LAST tool with cache_control. Anthropic semantics: a
	// cache_control marker is a "cache breakpoint" — everything BEFORE
	// it (including the marker block itself) gets cached together. So
	// marking the last tool caches the ENTIRE tools array (typically the
	// largest token block in any agent request — 5–10K tokens for a
	// 30-tool registry). The static system prefix is also under this
	// breakpoint via the buildSystemBlocks split below. claude-code's
	// `services/api/claude.ts:buildSystemPromptBlocks` does the same.
	if n := len(out.Tools); n > 0 {
		out.Tools[n-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
	}
	for _, m := range req.Messages {
		am := anthropicMessage{Role: string(m.Role)}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				am.Content = append(am.Content, anthropicContent{Type: "text", Text: c.Text})
			case "tool_use":
				am.Content = append(am.Content, anthropicContent{
					Type: "tool_use", ID: c.ToolUseID, Name: c.ToolName, Input: c.ToolInput,
				})
			case "tool_result":
				am.Content = append(am.Content, anthropicContent{
					Type: "tool_result", ToolUseID: c.ToolUseID, Content: c.ToolResult, IsError: c.IsError,
				})
			case "image":
				am.Content = append(am.Content, anthropicContent{
					Type: "image",
					Source: &anthropicImageSource{
						Type:      "base64",
						MediaType: c.MediaType,
						Data:      c.Data,
					},
				})
			case "thinking":
				// Intentionally dropped on send. Anthropic's
				// extended-thinking wire format requires the original
				// `signature` field on round-trip — without it the API
				// returns 400. metis currently captures the thinking
				// text via streaming but discards signature deltas (see
				// line 912 "signature_delta" — currently dropped), so
				// we can't re-emit a faithful thinking block yet.
				// Persisting the text in session jsonl + showing it in
				// the TUI is the user-visible win; signature plumbing
				// can come as a follow-up when extended-thinking gets
				// flipped on by default.
			}
		}
		out.Messages = append(out.Messages, am)
	}
	// Last user message cache (CC-B): mark the LAST content block of
	// the LAST user message with cache_control. The next turn's
	// request will hit cache for everything UP TO this point, so a
	// short follow-up question only re-bills the new user text. The
	// API allows up to 4 cache_control markers — we now use:
	//
	//   1. last tool                  (above)
	//   2. system static prefix       (buildSystemBlocks)
	//   3. system addendum            (buildSystemBlocks, optional)
	//   4. last user message          (here)
	//
	// claude-code's services/api/claude.ts has the same structure.
	// Tool-using turns specifically benefit because the long
	// tool_result blocks become part of the cached prefix on the
	// follow-up.
	if n := len(out.Messages); n > 0 {
		// Walk backwards to find the last TWO user messages so we can
		// place rolling cache checkpoints: the last user message is
		// the "leading edge" marker (next turn caches up through this
		// point); the second-to-last user message is the "trailing
		// anchor" so a long conversation's earlier history stays in
		// cache even after a couple of new turns.
		//
		// Cache breakpoint budget: tools, system, last-user, prior-user
		// = exactly 4. Matches Anthropic's hard cap (last 4 markers
		// retained; older ones silently dropped). claude-code's
		// services/api/claude.ts:buildCachePoints applies the same
		// dual-anchor scheme.
		lastUser, prevUser := -1, -1
		for i := n - 1; i >= 0; i-- {
			if out.Messages[i].Role != "user" {
				continue
			}
			if lastUser < 0 {
				lastUser = i
				continue
			}
			prevUser = i
			break
		}
		if lastUser >= 0 {
			cn := len(out.Messages[lastUser].Content)
			if cn > 0 {
				out.Messages[lastUser].Content[cn-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
			}
		}
		if prevUser >= 0 {
			cn := len(out.Messages[prevUser].Content)
			if cn > 0 {
				out.Messages[prevUser].Content[cn-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
			}
		}
	}
	return out
}

// SystemSection is the typed section form of the system prompt — a
// shadow of runtime.SystemPromptSection (this package can't import
// runtime). New callers pass []SystemSection to BuildSystemBlocksFromSections
// instead of relying on boundary-marker parsing in buildSystemBlocks.
//
// Cache=true emits cache_control on this section's block. Volatile=true
// overrides Cache and forces no-cache (used by sections that change
// every call — current token budget, streaming-status injection).
type SystemSection struct {
	Name     string
	Body     string
	Cache    bool
	Volatile bool
}

// chooseSystemBlocks routes to the typed-sections path when the
// caller populated req.SystemSections, falling back to the legacy
// boundary-marker string parser otherwise.
//
// The typed path wins because it preserves the Volatile flag —
// flattening to text + re-parsing markers can't tell "this is dynamic,
// never cache" from "this happens to be uncached today". Memory
// context relies on Volatile=true so a memory update doesn't
// invalidate the addendum cache (the bug the user hit:
// every turn writes a memory entry → addendum cache thrashes →
// no caching benefit at all).
func chooseSystemBlocks(req Request) []anthropicSystemBlock {
	if len(req.SystemSections) > 0 {
		secs := make([]SystemSection, 0, len(req.SystemSections))
		for _, s := range req.SystemSections {
			secs = append(secs, SystemSection{
				Name: s.Name, Body: s.Body, Cache: s.Cache, Volatile: s.Volatile,
			})
		}
		return BuildSystemBlocksFromSections(secs)
	}
	return buildSystemBlocks(req.System)
}

// BuildSystemBlocksFromSections is the typed-section construction
// path. Up to 4 cache_control markers in the result; Anthropic's
// total per-request budget is also 4, but the last-tool + last-user-
// message markers eat 2, so this function caps system-side at 2.
func BuildSystemBlocksFromSections(secs []SystemSection) []anthropicSystemBlock {
	out := make([]anthropicSystemBlock, 0, len(secs))
	cacheUsed := 0
	const maxSystemCache = 2
	for _, s := range secs {
		if s.Body == "" {
			continue
		}
		blk := anthropicSystemBlock{Type: "text", Text: s.Body}
		wantCache := s.Cache && !s.Volatile && cacheUsed < maxSystemCache
		if wantCache {
			blk.CacheControl = &anthropicCacheControl{Type: "ephemeral"}
			cacheUsed++
		}
		out = append(out, blk)
	}
	return out
}

// SystemPromptCacheBoundary marks the split between the static
// (cacheable) base prefix and the dynamic middle (env + project_context).
//
// Mirrors claude-code's __SYSTEM_PROMPT_DYNAMIC_BOUNDARY__ in
// constants/prompts.ts. The exact bytes don't matter, only that it's a
// unique never-occurring-in-real-text sentinel — keep it ASCII so it
// can't be partially consumed by ansi/grapheme processing.
const SystemPromptCacheBoundary = "<<<__METIS_CACHE_BOUNDARY__>>>"

// SystemPromptCacheBoundary2 marks the split between the dynamic
// middle and the second cacheable section (user addendum). Together
// with the first boundary, the system prompt is divided into 3
// sections:
//
//	[base                ] ← cached (slot 1)
//	[env + project_ctx   ] ← dynamic, no cache
//	[user addendum       ] ← cached (slot 2)
//
// Why two cache slots: the user addendum (~/.metis/system.md) is
// user-level state that's stable across cwd changes, so caching it
// separately survives `cd otherproj` even though the project_context
// in the middle invalidates. Anthropic's 4-breakpoint budget covers:
// last tool (1) + base (2) + addendum (3) + spare for last_user_msg.
//
// When this boundary is absent the addendum (if any) stays in the
// dynamic middle — backwards-compatible with assemblers that don't
// emit the second marker yet.
const SystemPromptCacheBoundary2 = "<<<__METIS_CACHE_BOUNDARY_2__>>>"

// buildSystemBlocks produces the array-form system field. Splitting
// rules:
//   - empty input → nil (omitempty drops the field, same as before)
//   - boundary present → [static (with cache_control), dynamic (no cache_control)]
//   - no boundary → [single block with cache_control on it] so callers
//     that don't yet emit the boundary still get tools+system cached
//     together as one breakpoint.
//
// We never emit more than 2 system blocks because Anthropic limits to 4
// cache breakpoints total (1 = last tool, 1 = static system, 1 = last
// user message in the future, 1 = spare).
func buildSystemBlocks(sys string) []anthropicSystemBlock {
	if sys == "" {
		return nil
	}
	idx := strings.Index(sys, SystemPromptCacheBoundary)
	if idx < 0 {
		return []anthropicSystemBlock{{
			Type: "text",
			Text: sys,
			// No cache_control here: the last-tool marker already creates
			// a breakpoint covering tools+system together. Adding a
			// second marker on a single-block system would burn a
			// breakpoint slot for zero additional benefit.
		}}
	}
	static := strings.TrimRight(sys[:idx], "\n ")
	rest := strings.TrimLeft(sys[idx+len(SystemPromptCacheBoundary):], "\n ")
	out := []anthropicSystemBlock{}
	if static != "" {
		out = append(out, anthropicSystemBlock{
			Type:         "text",
			Text:         static,
			CacheControl: &anthropicCacheControl{Type: "ephemeral"},
		})
	}
	// Check for boundary 2 — splits the rest into [dynamic, addendum].
	if idx2 := strings.Index(rest, SystemPromptCacheBoundary2); idx2 >= 0 {
		dynamic := strings.TrimRight(rest[:idx2], "\n ")
		addendum := strings.TrimLeft(rest[idx2+len(SystemPromptCacheBoundary2):], "\n ")
		if dynamic != "" {
			out = append(out, anthropicSystemBlock{
				Type: "text",
				Text: dynamic,
				// No cache_control: env + project_context vary per call.
			})
		}
		if addendum != "" {
			out = append(out, anthropicSystemBlock{
				Type:         "text",
				Text:         addendum,
				CacheControl: &anthropicCacheControl{Type: "ephemeral"},
			})
		}
	} else if rest != "" {
		// Single dynamic block — boundary 2 absent.
		out = append(out, anthropicSystemBlock{
			Type: "text",
			Text: rest,
		})
	}
	return out
}

func fromAnthropic(resp anthropicResp) *Response {
	out := &Response{
		StopReason:               resp.StopReason,
		InputTokens:              resp.Usage.InputTokens,
		OutputTokens:             resp.Usage.OutputTokens,
		CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
	}
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			out.Content = append(out.Content, ContentBlock{Type: "text", Text: c.Text})
		case "tool_use":
			out.Content = append(out.Content, ContentBlock{
				Type: "tool_use", ToolUseID: c.ID, ToolName: c.Name, ToolInput: c.Input,
			})
		}
	}
	return out
}

// --- Provider impl ---

func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	if a.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set ANTHROPIC_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	body := toAnthropicWithFlags(req, a.Model, a.MaxTokens, a.AntiDistillation, a.ClientSideDecoys)
	body.Stream = false

	var ar anthropicResp
	doOnce := func(b anthropicReq) error {
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return transport.RetryWithBackoff(ctx, 3, 0, func() error {
			httpReq, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/messages", bytes.NewReader(buf))
			if err != nil {
				return err
			}
			a.setHeaders(httpReq)
			resp, err := a.httpClient.Do(httpReq)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			rb, _ := io.ReadAll(resp.Body)
			if resp.StatusCode >= 400 {
				httpErr := fmt.Errorf("anthropic %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
				if transport.IsRetryableStatus(resp.StatusCode) {
					return &transport.RetryableError{Err: httpErr}
				}
				return httpErr
			}
			if err := json.Unmarshal(rb, &ar); err != nil {
				return fmt.Errorf("decode anthropic response: %w", err)
			}
			return nil
		})
	}

	err := doOnce(body)
	for attempt := 1; err != nil && isInvalidToolArgsError(err) && attempt <= 2; attempt++ {
		// MiniMax-style gateway reject — model emitted unparseable
		// JSON for tool arguments. Add an increasingly forceful
		// system reminder and retry. Second retry uses a stronger
		// reminder; we cap at 2 attempts so a persistently broken
		// path doesn't loop forever.
		debugRetryArgs(attempt, "Complete")
		err = doOnce(withToolArgsReminder(body))
	}
	if err != nil {
		return nil, err
	}
	return fromAnthropic(ar), nil
}

func (a *Anthropic) Stream(ctx context.Context, req Request) (StreamReader, error) {
	if a.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set ANTHROPIC_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	body := toAnthropicWithFlags(req, a.Model, a.MaxTokens, a.AntiDistillation, a.ClientSideDecoys)
	body.Stream = true

	// Retry the initial request (DNS / 429 / 5xx). Once we have a streaming
	// body we don't retry — partial SSE consumption can't be re-played.
	var resp *http.Response
	openOnce := func(b anthropicReq) error {
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return transport.RetryWithBackoff(ctx, 3, 0, func() error {
			httpReq, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/messages", bytes.NewReader(buf))
			if err != nil {
				return err
			}
			a.setHeaders(httpReq)
			resp, err = a.httpClient.Do(httpReq)
			if err != nil {
				return err
			}
			if resp.StatusCode >= 400 {
				rb, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				bodyStr := string(rb)
				httpErr := fmt.Errorf("anthropic %d: %s", resp.StatusCode, transport.Truncate(bodyStr, 500))
				// MiniMax-shim hints: when the upstream is MiniMax's
				// `/anthropic` endpoint, the body almost always carries a
				// `(NNNN)` business code. Translate the well-known ones to
				// actionable hints so the user sees "quota: insufficient
				// credits" instead of "anthropic 400: ...(1028)". The
				// original error is still wrapped, so `errors.Is` checks
				// still work.
				httpErr = wrapWithMinimaxHint(httpErr, bodyStr)
				if transport.IsRetryableStatus(resp.StatusCode) {
					return &transport.RetryableError{Err: httpErr}
				}
				return httpErr
			}
			return nil
		})
	}

	err := openOnce(body)
	for attempt := 1; err != nil && isInvalidToolArgsError(err) && attempt <= 2; attempt++ {
		// MiniMax-style gateway reject (e.g. error code 2013, "invalid
		// function arguments json string") — the upstream model emitted
		// unparseable JSON for `arguments` on a tool call (most common
		// for tools with no required fields). Add a system reminder
		// and retry. Some models need >1 retry before the empty-args
		// code path stops firing; cap at 2 so persistent failures
		// don't loop forever.
		debugRetryArgs(attempt, "Stream")
		err = openOnce(withToolArgsReminder(body))
	}
	if err != nil {
		return nil, err
	}
	return newAnthropicStream(resp.Body), nil
}

// debugRetryArgs emits a one-line stderr breadcrumb on each
// args-fixup retry. Gated on METIS_DEBUG=1 so production paths stay
// quiet but operators chasing a flaky tool-call setup can see retry
// activity without rebuilding.
func debugRetryArgs(attempt int, where string) {
	if os.Getenv("METIS_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "metis: %s: retry %d after invalid-tool-args 400 (adding system reminder)\n", where, attempt)
	}
}

// isInvalidToolArgsError detects the MiniMax-style upstream error that
// fires when the served model emits unparseable JSON for a tool call's
// `arguments` field. Observed verbatim against
// https://api.minimaxi.com/anthropic with MiniMax-M2.7:
//
//	anthropic 400: {"type":"error","error":{"type":"invalid_request_error",
//	  "message":"invalid params, invalid function arguments json string,
//	  tool_call_id: ... (2013)"}}
//
// The (2013) is MiniMax's internal error code; the "json string"
// wording is the load-bearing signal because it distinguishes this
// case from generic malformed-request 400s. We match on the literal
// substring so the detection is independent of any particular error
// envelope shape — Anthropic native, OpenAI-compat, and MiniMax all
// surface the message text in the response body which we already
// embed verbatim into the returned error.
func isInvalidToolArgsError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "invalid function arguments")
}

// withToolArgsReminder returns a copy of body with an extra system
// block reminding the model to emit valid non-empty JSON for tool
// arguments. Used as a one-shot fixup after isInvalidToolArgsError
// hits — the reminder shifts the model off whatever empty-args code
// path produced the unparseable output on the first try. Idempotent:
// safe to call on a body that already carries the reminder (we still
// append; redundant blocks don't change behavior). The original body
// is not mutated so subsequent retries can also call this.
func withToolArgsReminder(body anthropicReq) anthropicReq {
	out := body
	// Copy the System slice so appending doesn't alias the caller's.
	if len(body.System) > 0 {
		out.System = append([]anthropicSystemBlock(nil), body.System...)
	}
	out.System = append(out.System, anthropicSystemBlock{
		Type: "text",
		Text: "When invoking tools, the `arguments` object MUST be valid non-empty JSON. " +
			"For tools whose schema requires the field `_`, pass `\"_\": \"\"`. " +
			"For tools with no required fields, pass `{}` literally. " +
			"Never emit `null`, an empty string, or unquoted JSON for tool arguments.",
	})
	return out
}

func (a *Anthropic) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", a.APIKey)
	r.Header.Set("anthropic-version", "2023-06-01")
	if a.Beta != "" {
		r.Header.Set("anthropic-beta", a.Beta)
	}
}

// --- SSE stream ---

type anthropicStream struct {
	body io.ReadCloser
	sse  *sse.Reader
	// transient state for tool_use blocks (we accumulate input_json_delta)
	currentBlocks map[int]*streamBlock
}

type streamBlock struct {
	Type      string
	ToolUseID string
	ToolName  string
	JSONBuf   strings.Builder

	// Prefill captures the model's `input` field carried in
	// content_block_start. Anthropic protocol allows the model to send
	// the entire arguments object inline at block-start with NO
	// subsequent input_json_delta events ("prefill mode"). We keep it
	// separate from JSONBuf to handle the prefill+delta hybrid case
	// safely: when deltas arrive after a prefill, JSONBuf wins (the
	// deltas are the streaming form of the same data and will be
	// complete on their own); when no delta ever arrives, Prefill is
	// used at content_block_stop.
	Prefill string
}

func newAnthropicStream(body io.ReadCloser) *anthropicStream {
	return &anthropicStream{
		body:          body,
		sse:           sse.NewReader(body),
		currentBlocks: make(map[int]*streamBlock),
	}
}

func (s *anthropicStream) Close() error { return s.body.Close() }

func (s *anthropicStream) Recv() (StreamEvent, error) {
	for {
		frame, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return StreamEvent{Type: "message_stop"}, io.EOF
			}
			return StreamEvent{Type: "error", Err: err}, err
		}
		// Anthropic's `[DONE]` sentinel ends the stream cleanly.
		if frame.Data == "[DONE]" {
			return StreamEvent{Type: "message_stop"}, io.EOF
		}

		var env struct {
			Type         string          `json:"type"`
			Index        int             `json:"index"`
			Delta        json.RawMessage `json:"delta"`
			ContentBlock json.RawMessage `json:"content_block"`
			Message      json.RawMessage `json:"message"`
			Usage        struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &env); err != nil {
			continue
		}

		switch env.Type {
		case "message_start":
			// usage info available; emit empty event with token counts
			var msg struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			}
			_ = json.Unmarshal(env.Message, &msg)
			return StreamEvent{
				Type:                     "message_start",
				InputTokens:              msg.Usage.InputTokens,
				OutputTokens:             msg.Usage.OutputTokens,
				CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
			}, nil
		case "content_block_start":
			var cb struct {
				Type  string         `json:"type"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			}
			_ = json.Unmarshal(env.ContentBlock, &cb)
			blk := &streamBlock{Type: cb.Type, ToolUseID: cb.ID, ToolName: cb.Name}
			// Capture prefill: if the model sent the whole arguments
			// object inline at block-start (instead of via deltas), we
			// stash it in Prefill. content_block_stop falls back to
			// Prefill when no input_json_delta arrived.
			if cb.Type == "tool_use" && len(cb.Input) > 0 {
				if b, err := json.Marshal(cb.Input); err == nil {
					blk.Prefill = string(b)
				}
			}
			s.currentBlocks[env.Index] = blk
			if cb.Type == "tool_use" {
				return StreamEvent{Type: "tool_use_start", ToolUseID: cb.ID, ToolName: cb.Name}, nil
			}
			continue
		case "content_block_delta":
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
			}
			_ = json.Unmarshal(env.Delta, &d)
			blk := s.currentBlocks[env.Index]
			if d.Type == "text_delta" {
				return StreamEvent{Type: "text_delta", TextDelta: d.Text}, nil
			}
			// Extended-thinking: Anthropic emits the reasoning trace as
			// thinking_delta blocks separately from text. Forward them so
			// the UI can render dim/italic alongside the final answer.
			// Signature deltas (cryptographic block signature) are
			// internal — drop them rather than streaming binary noise.
			if d.Type == "thinking_delta" {
				return StreamEvent{Type: "thinking_delta", TextDelta: d.Thinking}, nil
			}
			if d.Type == "signature_delta" {
				continue
			}
			if d.Type == "input_json_delta" && blk != nil {
				blk.JSONBuf.WriteString(d.PartialJSON)
				return StreamEvent{Type: "tool_input_delta", ToolUseID: blk.ToolUseID, InputDelta: d.PartialJSON}, nil
			}
			continue
		case "content_block_stop":
			blk := s.currentBlocks[env.Index]
			if blk != nil && blk.Type == "tool_use" {
				// Pick the best-available source for the tool's JSON
				// arguments at this stop: deltas if any arrived (the
				// streaming form), else the prefill captured at
				// content_block_start. If both are empty (the model
				// emitted no input at all — legal for zero-arg tools
				// like MetisInfo with all-optional schema), the
				// downstream agent will marshal as `{}` thanks to the
				// MarshalJSON fix on anthropicContent.
				input := blk.JSONBuf.String()
				if input == "" {
					input = blk.Prefill
				}
				ev := StreamEvent{Type: "tool_use_stop", ToolUseID: blk.ToolUseID, InputDelta: input}
				delete(s.currentBlocks, env.Index)
				return ev, nil
			}
			delete(s.currentBlocks, env.Index)
			continue
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(env.Delta, &d)
			// Forward both input + output tokens. Anthropic native usually
			// reports input_tokens at message_start and only output_tokens
			// at message_delta, but Anthropic-compatible gateways are
			// inconsistent — MiniMax in particular reports the input
			// count here, not at the start. Carrying both means the
			// downstream tracker doesn't end up with `in == 0` on those
			// providers and the bottom-right counter actually moves.
			return StreamEvent{
				Type:                     "message_delta",
				StopReason:               d.StopReason,
				InputTokens:              env.Usage.InputTokens,
				OutputTokens:             env.Usage.OutputTokens,
				CacheCreationInputTokens: env.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     env.Usage.CacheReadInputTokens,
			}, nil
		case "message_stop":
			return StreamEvent{Type: "message_stop"}, io.EOF
		case "error":
			return StreamEvent{Type: "error", Err: errors.New(frame.Data)}, nil
		}
		// Unknown event type — keep reading.
	}
}
