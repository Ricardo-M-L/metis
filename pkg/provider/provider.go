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
// One of Text / ToolUse / ToolResult is populated.
type ContentBlock struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	ToolUseID  string         `json:"tool_use_id,omitempty"`
	ToolName   string         `json:"name,omitempty"`
	ToolInput  map[string]any `json:"input,omitempty"`
	ToolResult string         `json:"content,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
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
}

// Request is a complete chat completion request.
//
// MaxTokens, when non-zero, overrides the per-provider default; the agent
// loop uses this to dial down output budget for /fast mode without
// rebuilding the provider client. Effort is the cross-provider reasoning
// intensity dial (Anthropic thinking budget / OpenAI reasoning_effort);
// the canonical type lives in pkg/llm.
type Request struct {
	Model         string
	System        string
	Messages      []Message
	Tools         []ToolSpec
	MaxTokens     int
	Temperature   float64
	Stream        bool
	StopSequences []string
	Effort        llm.Effort
}

// StreamEvent is one chunk emitted while streaming a response.
// Type is one of: "text_delta", "tool_use_start", "tool_input_delta",
// "tool_use_stop", "message_stop", "error".
type StreamEvent struct {
	Type         string
	TextDelta    string
	ToolUseID    string
	ToolName     string
	InputDelta   string // partial JSON for tool input
	StopReason   string
	InputTokens  int
	OutputTokens int
	Err          error
}

// Response is the final aggregated result of a (possibly streamed)
// completion. StopReason is one of: "end_turn" / "tool_use" /
// "max_tokens" / "stop_sequence".
type Response struct {
	Content      []ContentBlock
	StopReason   string
	InputTokens  int
	OutputTokens int
}

// Provider is the abstraction every LLM backend implements. Stream emits
// StreamEvents; non-streaming callers use Complete.
//
// MaxContextTokens returns the context window size for the current
// model — used by the agent loop's compactor to decide when to trigger
// context summarization.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (StreamReader, error)
	MaxContextTokens() int
}

// StreamReader is a typed iterator over StreamEvents. Close releases
// any underlying connection — callers MUST Close, even on early exit.
type StreamReader interface {
	io.Closer
	Recv() (StreamEvent, error) // returns io.EOF at end
}
