// Package provider exposes the public LLM Provider contract a 3rd-party
// plugin implements to add a new chat completion backend.
//
// Pairs with pkg/tool, pkg/hook, pkg/channel, pkg/skill, pkg/memory,
// pkg/llm — completing Metis's plugin SDK. A provider plugin needs to
// satisfy four methods (Name / Complete / Stream / MaxContextTokens),
// translating the provider-neutral Request type to its wire format and
// streaming back via a StreamReader.
//
// Authoring sketch:
//
//	package mistral
//	import (
//	    "context"
//	    "github.com/Ricardo-M-L/metis/pkg/provider"
//	)
//
//	type Mistral struct{ apiKey string }
//
//	func (m *Mistral) Name() string             { return "mistral" }
//	func (m *Mistral) MaxContextTokens() int    { return 128_000 }
//	func (m *Mistral) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) { ... }
//	func (m *Mistral) Stream(ctx context.Context, req provider.Request) (provider.StreamReader, error) { ... }
//
// Wire-format types (Request / Response / Message / ContentBlock /
// ToolSpec / StreamEvent) are in this package so wrapper providers can
// inspect or rewrite them: e.g. a "fallback chain" provider that probes
// the primary and falls back; or a "cache" provider that keys on
// hash(req.Messages) and short-circuits when there's a hit.
package provider

import (
	"context"
	"io"

	"github.com/Ricardo-M-L/metis/pkg/llm"
)

// Role identifies the originator of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentBlock is the discriminated-union element of a Message.
// One of Text / ToolUse / ToolResult / Image is populated, keyed by Type.
//
// Image blocks (Type="image") carry a base64-encoded payload + IANA
// media type. Provider adapters translate to their wire format:
//
//   - Anthropic: {"type":"image", "source":{"type":"base64",
//     "media_type":<MediaType>, "data":<Data>}}
//   - OpenAI:    {"type":"image_url",
//     "image_url":{"url":"data:<MediaType>;base64,<Data>"}}
//
// Only attachments uploaded inline are supported here; URL-based image
// references would need a separate Source field.
type ContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	ToolName  string         `json:"name,omitempty"`
	ToolInput map[string]any `json:"input,omitempty"`
	// ToolInputMalformed is an in-memory parse-failure marker. Providers and
	// the stream consumer set it when function arguments are not valid JSON.
	// It is deliberately not persisted or sent back over provider wires: raw
	// malformed arguments may contain credentials, while the dispatcher only
	// needs the boolean fact in order to return a safe INVALID_JSON result.
	ToolInputMalformed bool   `json:"-"`
	ToolResult         string `json:"content,omitempty"`
	IsError            bool   `json:"is_error,omitempty"`
	// Display and Presentation are Metis UI metadata for tool_result blocks.
	// Provider adapters intentionally ignore them when constructing provider
	// wire payloads, while session JSON keeps them for faithful history replay.
	Display      string         `json:"display,omitempty"`
	Presentation map[string]any `json:"presentation,omitempty"`

	// ProviderHint carries opaque provider-specific blobs that must
	// round-trip across requests. Gemini-3.5+ uses
	// `gemini.thought_signature` — emitted by the model on parts
	// containing a function_call and required to be echoed back on
	// the corresponding history entry, else gemini rejects the next
	// turn. Other providers ignore unknown keys.
	ProviderHint map[string]string `json:"provider_hint,omitempty"`

	// Synthetic marks a text block the loop injected (repeat-tool
	// reminders, steer echoes, compaction checkpoints) rather than the human
	// typing it. Persisting provenance is required for exact history_replace
	// replay: otherwise a resumed checkpoint becomes an apparent user request
	// and may be selected as the active task by a later compaction.
	Synthetic bool `json:"synthetic,omitempty"`

	// Image-specific (Type="image"):
	MediaType string `json:"media_type,omitempty"` // e.g., "image/png"
	Data      string `json:"data,omitempty"`       // base64-encoded bytes

	// ToolResultBlocks is the multi-part body for a Type="tool_result"
	// block that mixes text + images (vision-aware tools like
	// ViewImage). Anthropic accepts this natively as
	// content=[{text}, {image}]; OpenAI-style adapters fan it out into
	// the role="tool" textual content + a follow-up user message
	// carrying the image_url parts. When empty, fall back to the
	// ToolResult string above. Skipped in JSON persistence to keep
	// the on-disk transcript format stable — vision payloads are
	// large and not worth replaying.
	ToolResultBlocks []ContentBlock `json:"-"`
}

// Message is a single turn in a conversation.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ToolSpec is the provider-neutral tool definition handed to the model.
// Provider adapters translate it into the wire-specific tool schema
// (Anthropic `tools[]` / OpenAI `functions[]` / etc.).
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	// Exposure is request-local routing metadata used to keep provider cache
	// boundaries aligned with the registry's stable Direct prefix. It is not
	// part of any provider wire format or persisted transcript.
	Exposure string `json:"-"`
}

// ResponseFormat asks providers with native structured-output support to
// constrain the final text to a JSON Schema. Providers that do not implement
// it may ignore the field; callers should still validate the result locally.
type ResponseFormat struct {
	Name        string
	Description string
	JSONSchema  map[string]any
	Strict      bool
}

// Request is a complete chat completion request.
//
// MaxTokens, when non-zero, overrides the per-provider default; the agent
// loop uses this to dial down output budget for /fast mode without
// rebuilding the provider client. Effort is the cross-provider reasoning
// intensity dial (Anthropic thinking budget / OpenAI reasoning_effort);
// the canonical type lives in pkg/llm.
type Request struct {
	Model    string
	System   string
	Messages []Message
	Tools    []ToolSpec
	// SystemSections is the typed-section form of System. When non-nil,
	// providers that support per-section caching (Anthropic) prefer
	// this over System and emit cache_control independently per
	// section. Memory context, env block, and project_context can each
	// have their own caching posture so a memory update doesn't blow
	// away the cache for the (otherwise stable) addendum and base
	// prefix.
	//
	// When nil, providers fall back to parsing System for the legacy
	// boundary markers (SystemPromptCacheBoundary). Sections kept as a
	// dedicated field rather than re-parsed because string round-trip
	// loses the Volatile flag — once flattened to text we can't tell
	// "this is dynamic, never cache" from "this happens to look
	// non-cacheable today".
	SystemSections []SystemSection
	MaxTokens      int
	Temperature    float64
	Stream         bool
	StopSequences  []string
	Effort         llm.Effort
	ResponseFormat *ResponseFormat
}

// SystemSection mirrors anthropic.SystemSection at the public-API
// boundary so callers can pass typed sections without importing the
// concrete provider package. Cache=true asks the provider to mark
// this section's block with cache_control (subject to the provider's
// per-request breakpoint budget); Volatile=true overrides Cache and
// forces no-cache, used by sections that change every call.
type SystemSection struct {
	Name     string
	Body     string
	Cache    bool
	Volatile bool
}

// StreamEvent is one chunk emitted while streaming a response.
// Type is one of: "text_delta", "tool_use_start", "tool_input_delta",
// "tool_use_stop", "message_stop", "error".
//
// InputTokens / CacheCreationInputTokens / CacheReadInputTokens are disjoint
// prompt-usage buckets. InputTokens excludes cached input even when an
// upstream wire format reports a total that includes it; providers without
// prompt caching leave both cache fields at 0. Consumers that compute
// context-window load add the three buckets exactly once.
type StreamEvent struct {
	Type                     string
	TextDelta                string
	ToolUseID                string
	ToolName                 string
	InputDelta               string // partial JSON for tool input
	StopReason               string
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	Err                      error
	// ProviderHint propagates opaque provider-specific blobs from a
	// streaming response back to ContentBlock.ProviderHint via the
	// stream consumer (see internal/agent/loop.go::consumeStream).
	// Currently used for gemini-3.5+ `thoughtSignature` on
	// tool_use_start events. Other providers ignore it.
	ProviderHint map[string]string
}

// Response is the final aggregated result of a (possibly streamed)
// completion. StopReason is one of: "end_turn" / "tool_use" /
// "max_tokens" / "stop_sequence".
type Response struct {
	Content                  []ContentBlock
	StopReason               string
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// Provider is the abstraction every LLM backend implements. Stream emits
// StreamEvents; non-streaming callers use Complete.
//
// MaxContextTokens returns the context window size for the current
// model — used by the agent loop's compactor to decide when to trigger
// context summarization.
//
// ModelID returns the actual model identifier the Provider sends on
// the wire — i.e. the value that lands in the request's `model` field.
// Distinct from any higher-level "what model did the user pick" string
// the TUI tracks separately: the provider's wire value is what actually
// executes, so it's the trustworthy source for the status bar (user
// screenshot 35 / 2026-05-17 surfaced a desync where the top bar showed
// "deepseek-v4-pro" but the running Anthropic-transport provider kept
// sending "minimax-m2.7"). Method named ModelID rather than Model so it
// doesn't collide with the existing exported `Model string` struct
// field on every concrete provider implementation.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (StreamReader, error)
	MaxContextTokens() int
	ModelID() string
}

// ContextHistoryPolicy is an optional provider capability used when turning a
// completed response into the next request's active-context estimate. Some
// transports persist reasoning for the UI but deliberately do not send those
// blocks back on the wire (Anthropic without signatures and OpenAI Responses
// are the common cases). Billing output_tokens still includes that reasoning,
// so treating every output token as future context can overstate the window by
// tens of thousands of tokens.
//
// Providers that do not implement this interface are assumed to replay every
// assistant block, preserving the historical/output-token fast path.
type ContextHistoryPolicy interface {
	ContextIncludesAssistantBlock(ContentBlock) bool
}

// StreamReader is a typed iterator over StreamEvents. Close releases
// any underlying connection — callers MUST Close, even on early exit.
type StreamReader interface {
	io.Closer
	Recv() (StreamEvent, error) // returns io.EOF at end
}

// VisionSupporter is an optional interface a Provider may implement
// to declare whether the active model accepts `image` content blocks.
// New callers should prefer ProviderVisionCapability so an absent declaration
// is not confused with an explicit negative.
//
// Optional rather than required so external embedders implementing
// only the core Provider interface don't break on upgrade. A provider
// that doesn't implement this is treated as "vision capability unknown".
type VisionSupporter interface {
	SupportsVision() bool
}

// VisionCapability is deliberately tri-state. A missing catalog entry or an
// external Provider that predates capability reporting is Unknown, not proof
// that the upstream model rejects images.
type VisionCapability uint8

const (
	VisionUnknown VisionCapability = iota
	VisionUnsupported
	VisionSupported
)

// VisionCapabilityReporter is the richer optional contract used by image
// gates. VisionSupporter remains supported for third-party providers compiled
// against the older boolean API.
type VisionCapabilityReporter interface {
	VisionCapability() VisionCapability
}

// ProviderVisionCapability returns the strongest capability declaration the
// provider exposes. Legacy providers with no declaration remain Unknown;
// callers may let the upstream API adjudicate rather than silently dropping
// user attachments.
func ProviderVisionCapability(p Provider) VisionCapability {
	if p == nil {
		return VisionUnsupported
	}
	if v, ok := p.(VisionCapabilityReporter); ok {
		return v.VisionCapability()
	}
	if v, ok := p.(VisionSupporter); ok {
		if v.SupportsVision() {
			return VisionSupported
		}
		return VisionUnsupported
	}
	return VisionUnknown
}

// ProviderSupportsVision is the backwards-compatible boolean shortcut. It is
// true only for an explicit Supported result; image submission gates that
// need to distinguish Unknown from Unsupported use ProviderVisionCapability.
func ProviderSupportsVision(p Provider) bool {
	return ProviderVisionCapability(p) == VisionSupported
}
