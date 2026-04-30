// Package llm holds the public, provider-agnostic types Metis exposes to
// 3rd-party plugins and adapters. Anything in this package is part of the
// stable API surface — internal/llm consumes these types and implements the
// transport-specific wire formats.
//
// The split is intentional: openclaw's `packages/plugin-sdk` and hermes-agent's
// `agent/anthropic_adapter.py + agent/bedrock_adapter.py` both keep wire-shape
// adapters separate from the abstract types they translate. We're doing the
// same in Go form: pkg/llm = abstract, internal/llm = transports.
package llm

// Effort is the user-visible reasoning intensity dial.
//
// It maps to provider-specific knobs in the adapter layer:
//   - Anthropic:  thinking.budget_tokens (low ~1k, medium ~4k, high ~16k)
//   - OpenAI:     reasoning_effort = "low" | "medium" | "high"
//   - Gemini:     thinking_config.thinking_budget (similar curve)
//
// An empty Effort means "use whatever the provider/model defaults to" — we
// deliberately don't override silently, because some models (e.g. Sonnet
// without thinking enabled) error out if a thinking budget is sent.
type Effort string

const (
	EffortDefault Effort = ""       // no override, use model/provider default
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
)

// Valid reports whether e is one of the recognized values.
// EffortDefault counts as valid — it's the "no opinion" sentinel.
func (e Effort) Valid() bool {
	switch e {
	case EffortDefault, EffortLow, EffortMedium, EffortHigh:
		return true
	}
	return false
}

// BudgetTokens returns the Anthropic thinking budget that corresponds to e.
// Returns 0 when e is EffortDefault (don't send a thinking field at all).
//
// Numbers chosen to match opencode's defaults (low ~= cheap quick draft,
// high ~= long-form deliberation). Adjust here if a provider rejects the size.
func (e Effort) BudgetTokens() int {
	switch e {
	case EffortLow:
		return 1024
	case EffortMedium:
		return 4096
	case EffortHigh:
		return 16384
	}
	return 0
}

// OpenAI returns the value to set in the openai-compat `reasoning_effort` field.
// Returns "" when no override should be sent.
func (e Effort) OpenAI() string {
	switch e {
	case EffortLow, EffortMedium, EffortHigh:
		return string(e)
	}
	return ""
}

// ParseEffort accepts the user-typed string from /effort and returns the
// canonical Effort. Unknown strings return EffortDefault + ok=false so the
// caller can show a help message rather than silently swallow the typo.
func ParseEffort(s string) (Effort, bool) {
	switch s {
	case "":
		return EffortDefault, true
	case "low", "l", "fast":
		return EffortLow, true
	case "medium", "m", "med", "mid":
		return EffortMedium, true
	case "high", "h", "max", "deep":
		return EffortHigh, true
	}
	return EffortDefault, false
}
