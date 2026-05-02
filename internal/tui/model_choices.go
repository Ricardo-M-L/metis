package tui

import "github.com/Ricardo-M-L/metis/internal/tui/screen"

// builtinModelChoices is the curated list shown in the /model picker.
// Hand-maintained rather than auto-discovered because the providers
// don't expose an enumerate endpoint and the model identifier strings
// have meaningful aliases (e.g. "claude-opus-4-7" vs the internal
// "claude-opus-4-7@20251015"). Each entry pairs the canonical model
// ID metis sends to the API with a human-friendly description.
//
// Adding a new model: append to this slice. Removing: drop the entry.
// Custom IDs not in this list are still settable via the inline form
// `/model <id>` — the picker is just for browsing the curated set.
var builtinModelChoices = []screen.ModelChoice{
	// Anthropic — current generation.
	{ID: "claude-opus-4-7", Description: "most capable, best for hard tasks", Provider: "anthropic"},
	{ID: "claude-sonnet-4-6", Description: "fast + smart, balanced", Provider: "anthropic"},
	{ID: "claude-haiku-4-5-20251001", Description: "cheapest, near-instant", Provider: "anthropic"},

	// MiniMax via Anthropic-compatible gateway (yunwu.ai etc.).
	{ID: "MiniMax-M2.7", Description: "open-weight, 192k window, low-cost", Provider: "minimax"},

	// Gemini.
	{ID: "gemini-2.5-pro", Description: "Google's flagship, 1M+ context", Provider: "gemini"},
	{ID: "gemini-2.0-flash", Description: "fast Gemini for high-throughput", Provider: "gemini"},

	// OpenAI.
	{ID: "gpt-4o", Description: "OpenAI flagship, multimodal", Provider: "openai"},
	{ID: "gpt-4o-mini", Description: "cheap OpenAI, good for simple tasks", Provider: "openai"},
}
