package tui

// theme_provider.go — per-provider color tinting (T9). Inspired by
// crush internal/ui/styles/themes.go ThemeForProvider(): instead of
// hard-coding accents, derive them from the active provider so users
// can tell at a glance which model they're talking to.
//
// The base palette stays under user control via METIS_THEME (dark /
// light / dark-daltonized). This file only retints a couple of accent
// fields when the user opts in — explicit /theme picks remain a
// no-tint override.

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// providerAccents maps provider id → primary brand accent (hex).
// Lowercase keys; lookup is case-insensitive. Add new providers as
// they ship — the renderer falls back to currentTheme.AccentBlue when
// the id is unknown so this map staying out-of-date never breaks
// rendering.
//
// Stored as hex strings (not pre-built colors) because lipgloss.Color
// is a function in v2; the lookup site does the conversion.
var providerAccents = map[string]string{
	"anthropic":  "#d97757", // claude orange (anthropic.com brand)
	"openai":     "#10a37f", // openai green (chatgpt brand)
	"gemini":     "#4285f4", // google blue
	"deepseek":   "#9b6dff", // deepseek purple
	"moonshot":   "#1e88e5", // kimi blue
	"kimi":       "#1e88e5", // kimi alias
	"minimax":    "#ff6b6b", // minimax red
	"alibaba":    "#ff7a00", // qwen orange
	"qwen":       "#ff7a00", // alias
	"mistral":    "#ff5722", // mistral orange-red
	"groq":       "#f55036", // groq red-orange
	"cohere":     "#39594d", // cohere muted teal
	"together":   "#0080ff", // together blue
	"openrouter": "#7c4dff", // openrouter violet
}

// ProviderAccentTint returns the brand color for providerID. Returns
// the active theme's AccentBlue when the id is empty or unknown, so
// callers can use this without an "is-known-provider" guard.
//
// The renderer typically calls this for elements that carry provider
// identity — the model glyph in the status bar, the assistant role
// chevron, the streaming spinner. Body text, errors, diffs etc keep
// reading from currentTheme to preserve theme intent.
func ProviderAccentTint(providerID string) color.Color {
	if hex, ok := providerAccents[strings.ToLower(strings.TrimSpace(providerID))]; ok {
		return lipgloss.Color(hex)
	}
	return currentTheme.AccentBlue
}

// ThemeForProvider returns a defensive copy of the active theme with
// its primary accent fields retinted for providerID. Use when a
// component needs a full Theme value rather than just a color (e.g.
// the welcome banner, which composes several styles in one render).
//
// When providerID is empty or unknown, returns currentTheme unchanged
// (callers should treat the result as read-only — same pointer).
func ThemeForProvider(providerID string) *Theme {
	tint, known := providerAccents[strings.ToLower(strings.TrimSpace(providerID))]
	if !known {
		return currentTheme
	}
	clone := *currentTheme
	clone.Name = currentTheme.Name + "+" + strings.ToLower(providerID)
	clone.AccentBlue = lipgloss.Color(tint)
	// Keep AccentCyan tinted too — the user-input marker reads cyan in
	// dark theme; matching it to the provider tint keeps the input
	// box visually parented to the model that will answer it.
	clone.AccentCyan = lipgloss.Color(tint)
	return &clone
}

// ApplyProviderTint retints the active currentTheme in place using
// providerID. Used by `/theme auto` (commands.go) so the user can
// opt into per-provider tinting without picking a fixed palette.
//
// This MUTATES currentTheme — call it after the user explicitly opts
// in, never as a side effect of routine renders. Callers must call
// initStyles() afterward to pick up the new accents.
//
// THREAD SAFETY: must be called from the main bubbletea Update
// goroutine. View() reads currentTheme without locking, so calling
// this from a tea.Cmd background goroutine would race with the
// renderer.
func ApplyProviderTint(providerID string) {
	tinted := ThemeForProvider(providerID)
	if tinted == currentTheme {
		return // unknown provider — leave the theme alone
	}
	currentTheme = tinted
}

// KnownProviderTints returns the provider ids this file knows about,
// in alphabetical order. Used by /theme help and tests; not part of
// the render hot path.
func KnownProviderTints() []string {
	out := make([]string, 0, len(providerAccents))
	for k := range providerAccents {
		out = append(out, k)
	}
	// Stable order so /theme help output is deterministic.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
