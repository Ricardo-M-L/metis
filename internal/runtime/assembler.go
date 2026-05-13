package runtime

// assembler.go — central composer for the base system prompt. Wraps
// the section registry (sections.go) with a small Run() that callers
// invoke per chat boot. Cleanly separates "what sections exist" from
// "what gets included for this boot."
//
// Two consumption paths today:
//
//   1. AssembleBaseSections(ctx) — returns []SystemPromptSection so
//      cmd/metis/main.go can interleave them with project_context /
//      addendum / env / allowed_dirs / overlays before rendering.
//
//   2. AssembleBaseString(ctx) — convenience that joins the same
//      sections with the canonical "\n\n" delimiter, matching
//      RenderBasePrompt's pre-section output for backward compat.
//
// The old monolithic base.md is kept on disk for now (read by the
// legacy basePromptTPL embed) so existing code paths that haven't
// migrated to AssembleBaseSections still work. New code should call
// the assembler.

import "strings"

// AssembleBaseSections runs every getter in DefaultSectionGetters
// against ctx and returns the non-skipped sections in order. Each
// section retains its Cache / Volatile flags so downstream Anthropic
// cache-control mapping still works per-section.
//
// Callers that need to inject extra sections (provider hint, plan
// overlay, project_context) should append to the returned slice —
// the assembler doesn't reach across the runtime boundary.
func AssembleBaseSections(ctx PromptCtx) []SystemPromptSection {
	getters := DefaultSectionGetters()
	out := make([]SystemPromptSection, 0, len(getters))
	for _, g := range getters {
		s := g(ctx)
		if s.Name == "" {
			continue // skipped
		}
		out = append(out, s)
	}
	return out
}

// AssembleBaseString renders the same set as AssembleBaseSections
// joined with double-newline separators. Output matches
// RenderBasePrompt's pre-section format (minus the trailing
// {{.ProviderHint}} — callers append that themselves as a separate
// section so it can be cached independently).
func AssembleBaseString(ctx PromptCtx) string {
	secs := AssembleBaseSections(ctx)
	parts := make([]string, 0, len(secs))
	for _, s := range secs {
		parts = append(parts, strings.TrimRight(s.Body, "\n"))
	}
	return strings.Join(parts, "\n\n")
}
