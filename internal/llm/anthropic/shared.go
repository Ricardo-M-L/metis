package anthropic

// Public wrappers around package-internal helpers that the vertex
// and bedrock subpackages reuse. They run the same Anthropic wire
// format over different transports (Vertex's GCP-auth predict
// endpoints, Bedrock's SigV4 InvokeModel), so they share the body
// translation + response parsing logic.
//
// We export thin aliases here rather than renaming the originals
// (toAnthropic/fromAnthropic/newAnthropicStream/anthropicReq/anthropicResp)
// to keep this Phase-2 refactor's diff small. The thin aliases
// document the cross-package API surface explicitly without forcing
// a rename across 700+ lines of anthropic.go.

import "io"

// Resp is the parsed response shape from Anthropic's Messages API
// non-streaming endpoint. Vertex/Bedrock decode their JSON bodies
// into this type and then call FromResponse.
type Resp = anthropicResp

// Req is the request envelope. Vertex/Bedrock build a copy via
// ToRequest, then mutate top-level fields (drop "model", add
// "anthropic_version") before sending.
type Req = anthropicReq

// SystemBlock mirrors the Anthropic system-block array shape.
type SystemBlock = anthropicSystemBlock

// Tool / Message / Content mirror the per-block shapes vertex/
// bedrock construct directly when bypassing ToRequest. Most callers
// just use ToRequest.
type (
	Tool        = anthropicTool
	WireMessage = anthropicMessage
	WireContent = anthropicContent
)

// ToRequest builds the Anthropic-format request body from the
// provider-neutral Request shape. Equivalent to toAnthropic but
// exported for cross-package use.
func ToRequest(req Request, model string, maxTokens int) Req {
	return toAnthropic(req, model, maxTokens)
}

// ToRequestWithFlags is the variant that respects per-provider
// anti-distillation toggles. Vertex/Bedrock don't currently use the
// distillation flags but we expose the function symmetrically.
//
// Vertex/Bedrock paths never hit the MiniMax `/anthropic` gateway, so
// the schema-placeholder injection is hard-coded off here. Direct
// Anthropic provider callers go through anthropic.go's Complete/Stream
// which compute the flag from a.BaseURL — that's where MiniMax detection
// actually matters.
func ToRequestWithFlags(req Request, model string, maxTokens int, antiDistill, clientDecoys bool) Req {
	return toAnthropicWithFlags(req, model, maxTokens, antiDistill, clientDecoys, false)
}

// FromResponse converts Anthropic's response envelope into the
// provider-neutral Response shape. Vertex/Bedrock call this after
// decoding their HTTP body.
func FromResponse(resp Resp) *Response {
	return fromAnthropic(resp)
}

// NewStream wraps a SSE response body in the streaming reader. The
// Vertex SSE format is byte-identical to direct Anthropic; Bedrock
// uses a synthetic stream via cloud event-stream so it doesn't call
// this. Public so vertex can use it.
func NewStream(body io.ReadCloser) StreamReader {
	return newAnthropicStream(body)
}
