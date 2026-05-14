package openai

// Public wrappers around package-internal helpers that the azure
// subpackage reuses. Azure speaks the OpenAI Chat Completions wire
// format; only its URL routing (deployment-based) and auth header
// (api-key vs Bearer) differ. These exports let azure call into the
// shared body builder + SSE parser without spelunking through
// package-private symbols.

import "io"

// Resp is the parsed non-streaming response envelope.
type Resp = oaiResp

// Choice is one entry in Resp.Choices.
type Choice = oaiChoice

// Req is the request body envelope. Azure builds it via ToRequest
// then sends as-is.
type Req = oaiReq

// WireMessage is the OpenAI per-choice message shape (not the
// provider-neutral Message — that one is `provider.Message` aliased
// at the top of openai.go). Naming is "Wire" to match the analogous
// alias in anthropic/shared.go.
type WireMessage = oaiMessage

// Usage exposes the response-side usage envelope (prompt/completion
// tokens plus the three cached-token wire shapes — see oaiUsage doc
// in openai.go for details). Azure decodes its response into Resp
// (alias of oaiResp) and Resp.Usage is this type.
type Usage = oaiUsage

// ToRequest builds the OpenAI-format chat completion body from the
// provider-neutral Request shape.
func ToRequest(req Request, model string, maxTokens int) Req {
	return toOpenAI(req, model, maxTokens)
}

// FromChoice converts one OpenAI Choice + Usage into the provider-
// neutral Response. Azure uses this on Choices[0] of its decoded
// body. Usage carries cached-token fields when the upstream reports
// a prefix-cache hit (OpenAI `prompt_tokens_details.cached_tokens`,
// DeepSeek `prompt_cache_hit_tokens`, Kimi `cached_tokens`); the
// returned Response.CacheReadInputTokens picks the first non-zero.
func FromChoice(c Choice, usage Usage) *Response {
	return fromOpenAIChoice(c, usage)
}

// NewStream wraps an SSE response body in the streaming reader.
// Azure SSE is byte-identical to OpenAI SSE.
func NewStream(body io.ReadCloser) StreamReader {
	return newOpenAIStream(body)
}
