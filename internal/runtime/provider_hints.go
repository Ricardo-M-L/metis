package runtime

// provider_hints.go — short, provider-specific guidance appended at the
// tail of base.md via the provider_hint section. The goal is NOT to
// lecture the model about its own architecture; it's to forward the
// small number of model-family quirks that affect tool behavior in
// metis.
//
// Phase D refactor (2026-05-14): replaced the static per-provider
// string map with a capability-based composition. Each
// (provider, model) tuple resolves to a ProviderCapabilities struct,
// and the final hint is assembled from atomic fragments by checking
// the booleans. Win: a long-context thinking model on a new vendor
// gets the right combination of hints without us writing a new
// per-provider string. The atoms are small and reusable.

import "strings"

// ProviderCapabilities flags model-family quirks that change how the
// model should be addressed at the prompt level. Each flag is meant
// to gate ONE hint atom in composeProviderHint().
//
// All fields are positive ("model has X") not negative — keeps the
// truth table easy to scan. When in doubt, set to false (no hint
// emitted) rather than guess wrong.
type ProviderCapabilities struct {
	// FamilyName is a user-readable label rendered in the hint header.
	// "Anthropic", "MiniMax M-series", "Moonshot Kimi", etc.
	FamilyName string

	// XMLPreferred reports that the model parses XML-style structure
	// tags cleanly inside a single message. Anthropic family +
	// derivatives. Lets metis suggest <thinking>, <example>, etc.
	XMLPreferred bool

	// AnthropicCompatTools reports that the model speaks Anthropic's
	// tool_use / tool_result content-block protocol. metis's internal
	// shape matches this; on OpenAI-style endpoints we translate.
	AnthropicCompatTools bool

	// HasThinkingBlocks reports that the model emits reasoning as a
	// distinct content block (Anthropic "thinking", DeepSeek
	// reasoning_content, Kimi thinking-turbo). Used to remind the
	// model NOT to leak CoT into the final answer.
	HasThinkingBlocks bool

	// EmptyArgsToolUseBug reports the MiniMax error 2013 quirk: a
	// tool_use block with empty input crashes the upstream. We tell
	// the model to always emit {} rather than nothing.
	EmptyArgsToolUseBug bool

	// LongContext reports a window ≥200K tokens. Used to remind the
	// model that token budget is generous but answer brevity targets
	// still apply.
	LongContext bool

	// Bilingual reports that the model is meant for users who
	// regularly mix Chinese and English (Kimi, MiniMax). Reminder to
	// mirror the user's language choice.
	Bilingual bool

	// VerboseByDefault reports the family tends to over-explain
	// without an explicit length cap. Reinforces the base prompt's
	// length targets.
	VerboseByDefault bool

	// EmptyStreamChunks reports the provider occasionally streams
	// empty-content payloads (Zhipu / GLM). Tells the model to ignore
	// rather than treat as signal.
	EmptyStreamChunks bool

	// PromptCachingActive reports that metis enables prompt caching
	// on the wire for this provider (Anthropic + compat). Useful as a
	// nudge to keep big stable instructions in early turns so cache
	// hit rate stays high.
	PromptCachingActive bool

	// HasReasoningSplit reports that reasoning arrives in a separate
	// `reasoning_content` field (DeepSeek-style) rather than inline
	// in the main content. Same effect as HasThinkingBlocks for the
	// "don't leak CoT" warning but worth a distinct atom because the
	// wire shape differs.
	HasReasoningSplit bool

	// OpenAIFunctionCalls reports OpenAI's function-calling JSON
	// shape (distinct from Anthropic's tool_use blocks). metis
	// translates between them downstream — this atom just tells the
	// model what wire shape its end of the API speaks.
	OpenAIFunctionCalls bool
}

// DetectProviderCapabilities maps a (provider, model) tuple to a
// capability set. Comparison is case-insensitive on provider; model
// is matched against substrings since upstream renames common.
//
// Adding a new provider: append a case to the switch, optionally
// peek at the model name for sub-variants. The returned struct gets
// assembled into a hint by composeProviderHint().
func DetectProviderCapabilities(provider, model string) ProviderCapabilities {
	p := strings.ToLower(strings.TrimSpace(provider))
	m := strings.ToLower(strings.TrimSpace(model))

	// Unwrap a custom:<wrapped> profile by recursing on the suffix.
	if strings.HasPrefix(p, "custom:") {
		if inner := strings.TrimPrefix(p, "custom:"); inner != "" && inner != p {
			return DetectProviderCapabilities(inner, model)
		}
	}

	switch {
	case strings.HasPrefix(p, "anthropic"):
		return ProviderCapabilities{
			FamilyName:           "Anthropic",
			XMLPreferred:         true,
			AnthropicCompatTools: true,
			HasThinkingBlocks:    true,
			PromptCachingActive:  true,
		}
	case strings.HasPrefix(p, "minimax"):
		return ProviderCapabilities{
			FamilyName:           "MiniMax M-series",
			AnthropicCompatTools: true, // metis routes MiniMax via /anthropic endpoint
			HasThinkingBlocks:    true,
			EmptyArgsToolUseBug:  true,
			LongContext:          true,
			Bilingual:            true,
		}
	case strings.HasPrefix(p, "deepseek"):
		return ProviderCapabilities{
			FamilyName:        "DeepSeek",
			HasReasoningSplit: true,
		}
	case strings.HasPrefix(p, "kimi") || strings.Contains(p, "moonshot"):
		c := ProviderCapabilities{
			FamilyName:  "Moonshot Kimi",
			LongContext: true,
			Bilingual:   true,
		}
		if strings.Contains(m, "thinking") {
			c.HasThinkingBlocks = true
		}
		return c
	case strings.HasPrefix(p, "openai"):
		// OpenAI is the reference for function-calling — flag it
		// explicitly so the composed hint at least mentions the wire
		// shape and the family name. Without OpenAIFunctionCalls the
		// composer would emit just a header + zero atoms (filtered).
		return ProviderCapabilities{
			FamilyName:          "OpenAI",
			OpenAIFunctionCalls: true,
		}
	case strings.HasPrefix(p, "gemini") || strings.HasPrefix(p, "google"):
		return ProviderCapabilities{
			FamilyName:       "Gemini",
			LongContext:      true,
			VerboseByDefault: true,
		}
	case strings.HasPrefix(p, "glm") || strings.HasPrefix(p, "zhipu"):
		return ProviderCapabilities{
			FamilyName:        "GLM / Zhipu",
			EmptyStreamChunks: true,
			Bilingual:         true,
		}
	}
	return ProviderCapabilities{}
}

// ProviderHintFor returns the provider-specific guidance fragment to
// append at the tail of the base prompt, or "" when no hint applies.
// Composed from DetectProviderCapabilities + composeProviderHint —
// see those for the atom-by-atom logic.
func ProviderHintFor(providerName, modelName string) string {
	caps := DetectProviderCapabilities(providerName, modelName)
	return composeProviderHint(caps)
}

// composeProviderHint walks a capability struct and builds the final
// hint string. Order of atoms matters for readability: family-level
// facts (XML, tool-call shape) first, then quirks (empty-args bug),
// then style reminders (terseness for verbose families).
//
// Returns "" when caps has no FamilyName — that signals "we don't
// recognize this provider; don't add noise."
func composeProviderHint(caps ProviderCapabilities) string {
	if caps.FamilyName == "" {
		return ""
	}
	var parts []string
	parts = append(parts, "# Provider notes ("+caps.FamilyName+")")

	// Anthropic-compat block: XML, tool_use shape, prompt cache.
	if caps.AnthropicCompatTools && caps.XMLPreferred {
		parts = append(parts, atomAnthropicShape(caps.PromptCachingActive))
	} else if caps.AnthropicCompatTools {
		parts = append(parts, atomAnthropicToolUseOnly)
	} else if caps.OpenAIFunctionCalls {
		parts = append(parts, atomOpenAIFunctionCalls)
	}

	// Empty-args quirk first — operational, will affect literally every
	// tool call until the model internalizes it.
	if caps.EmptyArgsToolUseBug {
		parts = append(parts, atomEmptyArgsBug)
	}

	// Thinking-block hygiene (only one of the two will fire — they're
	// the same advice for different wire shapes).
	switch {
	case caps.HasThinkingBlocks:
		parts = append(parts, atomThinkingBlocks)
	case caps.HasReasoningSplit:
		parts = append(parts, atomReasoningSplit)
	}

	// Soft style atoms.
	if caps.LongContext {
		parts = append(parts, atomLongContext)
	}
	if caps.Bilingual {
		parts = append(parts, atomBilingual)
	}
	if caps.VerboseByDefault {
		parts = append(parts, atomVerboseByDefault)
	}
	if caps.EmptyStreamChunks {
		parts = append(parts, atomEmptyStreamChunks)
	}

	// Strip the header when no atoms fire (FamilyName known but no
	// quirks attached — happens for plain OpenAI).
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// ---- Atoms ----------------------------------------------------------

func atomAnthropicShape(promptCache bool) string {
	body := "You're running through an Anthropic-compatible endpoint. Tool calls use the tool_use / tool_result content-block protocol. XML-style tags (`<thinking>`, `<example>`, `<file>`) parse cleanly inside a single message when you need structure."
	if promptCache {
		body += " Prompt caching is on for system + tool definitions — refer to cached context by reference rather than restating it."
	}
	return body
}

const atomAnthropicToolUseOnly = `Tool calls use Anthropic's tool_use / tool_result content-block protocol.`

const atomOpenAIFunctionCalls = `You're running through OpenAI's function-calling endpoint. Tool calls use the function-call shape (JSON arguments). Markdown renders well in metis's TUI — use it sparingly for structure (lists, code fences) but skip headers for short replies.`

const atomEmptyArgsBug = `Known quirk: tool_use blocks with empty input arguments trigger a 400 error (code 2013). If a tool takes no required args this turn, still emit ` + "`{}`" + ` (not nothing) and prefer not to call argument-less tools unless necessary.`

const atomThinkingBlocks = `Your model emits reasoning in dedicated thinking blocks, separate from the user-facing content. Keep the thinking focused; the user only sees the final answer unless they explicitly ask to inspect thinking. Long internal monologue burns tokens nobody reads.`

const atomReasoningSplit = `Reasoning arrives in a separate ` + "`reasoning_content`" + ` field, not interleaved with the user-facing ` + "`content`" + `. Long CoT is fine in reasoning; just don't leak it into the final answer — that's what the length budgets above cap.`

const atomLongContext = `Context window is generous (≥200K tokens). Budget is rarely the binding constraint, but the answer-brevity targets in the base prompt still apply — long context is for the model to read, not for it to output.`

const atomBilingual = `Users may write in Chinese or English; mirror the user's language choice in your reply. Tool inputs (paths, commands, JSON) stay in English regardless.`

const atomVerboseByDefault = `This model family tends to over-explain by default. Hold yourself strictly to the length targets above (≤4 lines final answer, ≤25 words between tool calls). When in doubt, cut, don't add.`

const atomEmptyStreamChunks = `Streaming chunks occasionally arrive with empty content payloads — that's the provider, not a bug; the harness handles them. Don't treat empty deltas as a signal to retry.`
