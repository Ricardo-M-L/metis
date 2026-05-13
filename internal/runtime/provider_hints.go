package runtime

// provider_hints.go — short, provider-specific guidance appended at the
// tail of base.md.tpl via {{.ProviderHint}}. The goal is NOT to lecture
// the model about its own architecture; it's to forward the small
// number of model-family quirks that affect tool behavior in metis.
//
// Each fragment is intentionally ≤15 lines. Anything bigger probably
// belongs in a separate skill or agent profile, not in every system
// prompt for that provider.
//
// Selection: callers pass (providerName, modelName). providerName is
// the metis-side label ("anthropic", "minimax", "deepseek", "kimi",
// "openai", "gemini", "glm", "custom:<name>"). modelName is the
// upstream model id ("MiniMax-M2.7", "claude-opus-4-7", ...). We pick
// by providerName first, then peek at modelName for a couple of
// specific overrides (e.g. kimi-k2-thinking-turbo).

import "strings"

// ProviderHintFor returns the provider-specific guidance fragment to
// append at the tail of the base prompt, or "" when no hint applies.
//
// Comparison is case-insensitive on providerName. modelName is matched
// against substrings, not equality — upstream renames common.
func ProviderHintFor(providerName, modelName string) string {
	p := strings.ToLower(strings.TrimSpace(providerName))
	m := strings.ToLower(strings.TrimSpace(modelName))

	switch {
	case strings.HasPrefix(p, "anthropic"):
		return hintAnthropic
	case strings.HasPrefix(p, "minimax"):
		return hintMiniMax
	case strings.HasPrefix(p, "deepseek"):
		return hintDeepSeek
	case strings.HasPrefix(p, "kimi") || strings.Contains(p, "moonshot"):
		if strings.Contains(m, "thinking") {
			return hintKimi + "\n\n" + hintKimiThinking
		}
		return hintKimi
	case strings.HasPrefix(p, "openai"):
		return hintOpenAI
	case strings.HasPrefix(p, "gemini") || strings.HasPrefix(p, "google"):
		return hintGemini
	case strings.HasPrefix(p, "glm") || strings.HasPrefix(p, "zhipu"):
		return hintGLM
	case strings.HasPrefix(p, "custom:"):
		// Custom profiles map to whichever upstream family they wrap.
		// Try the suffix (e.g. "custom:kimi" → "kimi").
		suffix := strings.TrimPrefix(p, "custom:")
		if suffix != "" && suffix != p {
			return ProviderHintFor(suffix, modelName)
		}
	}
	return ""
}

// ---- Anthropic ------------------------------------------------------

const hintAnthropic = `# Provider notes (Anthropic)

You're running through Anthropic's Messages API. Prompt caching is on
for system + tool definitions, so the cheaper move is to NOT restate
context that's already cached — refer to it by reference. XML-style
tags (` + "`<thinking>`, `<example>`" + `, ` + "`<file>`" + `) parse cleanly when you
need structure inside a single message. Use ` + "`stop_reason`" + ` semantics:
once you emit a tool_use block, stop talking and wait for the result.`

// ---- MiniMax --------------------------------------------------------

const hintMiniMax = `# Provider notes (MiniMax M-series)

You're running through MiniMax's Anthropic-compatible endpoint. Two
known quirks:

  1. Empty-args tool_use blocks trigger a 400 (error code 2013). If a
     tool takes no required args this turn, still emit ` + "`{}`" + ` (not nothing)
     and prefer not to call argument-less tools unless necessary.
  2. Thinking output streams as a separate ` + "`thinking`" + ` content block,
     not interleaved with text. Keep reasoning concise; long internal
     monologue burns tokens the user never sees.

Tool-call format follows Anthropic's tool_use / tool_result shape.`

// ---- DeepSeek -------------------------------------------------------

const hintDeepSeek = `# Provider notes (DeepSeek)

You're running through DeepSeek's OpenAI-compatible endpoint. Reasoning
arrives in a ` + "`reasoning_content`" + ` field separate from the user-facing
` + "`content`" + ` — your <think> blocks are extracted automatically. Tool calls
use the OpenAI function-calling shape. Long CoT is fine; just don't
leak it into the final answer — that's what the budget targets above
cap.`

// ---- Kimi (Moonshot) ------------------------------------------------

const hintKimi = `# Provider notes (Kimi / Moonshot)

You're running through Moonshot's API. Tool calls use the OpenAI
function-calling shape. The user may be writing in Chinese or English
— mirror their language. Long-context (200K) means context budget is
generous, but answer brevity targets still apply.`

const hintKimiThinking = `Your model variant emits reasoning in dedicated thinking blocks. Keep
the thinking focused on the task; the user only sees the final answer
unless they explicitly ask to inspect thinking.`

// ---- OpenAI ---------------------------------------------------------

const hintOpenAI = `# Provider notes (OpenAI)

You're running through OpenAI's Responses or Chat Completions API.
Tool calls use the function-calling shape (JSON args). Markdown
renders well in metis's TUI — use it sparingly for structure (lists,
code fences) but skip headers for short replies.`

// ---- Gemini ---------------------------------------------------------

const hintGemini = `# Provider notes (Gemini)

You're running through Google's Gemini API. Models in this family tend
to over-explain by default — hold yourself strictly to the length
targets above (≤4 lines final answer, ≤25 words between tool calls).
Tool calls use Gemini's function_call format; metis translates them
into the common tool_use shape downstream.`

// ---- GLM (Zhipu) ----------------------------------------------------

const hintGLM = `# Provider notes (GLM / Zhipu)

You're running through Zhipu's OpenAI-compatible endpoint. Streaming
chunks occasionally arrive with empty content payloads — that's the
provider, not a bug; ignore them. Tool calls use the OpenAI shape.`
