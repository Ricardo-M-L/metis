package agent

// compact_tier.go implements the openclaude-style "compressToolHistory"
// concept on top of metis's existing Snip/Microcompact stack. The
// gist (per openclaude `compressToolHistory.ts`): the right amount
// to truncate an older tool_result depends on the effective context
// window. A model with a 200k window can comfortably keep raw tool
// outputs around for 30+ turns; a 16k DeepSeek instance starts
// hemorrhaging context after 5.
//
// Why this matters for metis specifically: the user's default
// provider is MiniMax (Anthropic-shim) and they also have DeepSeek /
// Kimi configured as OpenAI-compat — none of those advertise
// prompt-cache support, so old tool_results sit fully in every
// request body. A single 1024-char `git diff` can blow 30% of a 16k
// window if it's quoted across 6 turns.
//
// Implementation choice: rather than rewrite Snip, we override its
// per-block char cap based on the active provider's effective input
// cap. The existing Snip walk (compact.go::Snip) keeps doing its
// thing; only the threshold it uses tightens when the window is
// small.

import "math"

// CompressionTier is the per-tier knob set returned by tierForWindow.
// Each field maps to an existing Compactor option so the caller can
// keep using the existing Snip/Microcompact paths and just tighten
// thresholds when the window demands it.
//
// Why only two knobs (SnipMaxToolResultChars + SnipThreshold) and not
// also ProtectLast: ProtectLast is a UX/safety setting (how many
// recent turns the agent gets to read verbatim before it starts
// re-issuing tool calls), and the right value is governed by user
// preference, not by the model's window size. Snip thresholds, by
// contrast, are pure budget arithmetic — small window means small
// per-block char cap.
type CompressionTier struct {
	// SnipMaxToolResultChars overrides Compactor.SnipMaxToolResultChars
	// when this tier is active.
	SnipMaxToolResultChars int
	// SnipThreshold overrides Compactor.SnipThreshold (fraction of
	// effective input cap at which Snip fires).
	SnipThreshold float64
	// Name is a short human label for /doctor surfaces.
	Name string
}

// tierForWindow picks the right tier for a given effective input cap
// (already in tokens). The buckets mirror openclaude's seven tiers
// (compressToolHistory.ts) — each step roughly doubles the budget,
// so the snip thresholds halve.
//
// Rationale for the specific numbers:
//
//	16k:   keep ~200 chars per old block, snip very early (60% fill).
//	       This is the painful case — DeepSeek-V2 base, Ollama defaults.
//	32k:   400 chars / snip at 65%. Comfortable for chat, tight for code.
//	64k:   800 chars / snip at 70% (matches the existing "default" config
//	       — anything above 64k is already big enough that the historical
//	       defaults work fine, so the override turns into a no-op).
//	128k:  1500 chars / snip at 75%. Modern OpenAI / GPT-4o territory.
//	200k:  3000 chars / snip at 80%. Anthropic 200k window.
//	500k+: 6000 chars / snip at 85%. Future-proof for long-context.
//
// Boundary choice: openclaude's tiers were 16/32/64/128/256/512/1m;
// we collapse 256k into the 200k bucket because no current provider
// publishes a 256k window.
func tierForWindow(effectiveInputCapTokens int) CompressionTier {
	switch {
	case effectiveInputCapTokens <= 0:
		// Defensive: caller didn't know — keep existing defaults.
		return CompressionTier{Name: "unknown"}
	case effectiveInputCapTokens <= 16_000:
		return CompressionTier{
			Name:                   "tier-16k",
			SnipMaxToolResultChars: 200,
			SnipThreshold:          0.60,
		}
	case effectiveInputCapTokens <= 32_000:
		return CompressionTier{
			Name:                   "tier-32k",
			SnipMaxToolResultChars: 400,
			SnipThreshold:          0.65,
		}
	case effectiveInputCapTokens <= 64_000:
		return CompressionTier{
			Name:                   "tier-64k",
			SnipMaxToolResultChars: 800,
			SnipThreshold:          0.70,
		}
	case effectiveInputCapTokens <= 128_000:
		return CompressionTier{
			Name:                   "tier-128k",
			SnipMaxToolResultChars: 1500,
			SnipThreshold:          0.75,
		}
	case effectiveInputCapTokens <= 200_000:
		return CompressionTier{
			Name:                   "tier-200k",
			SnipMaxToolResultChars: 3000,
			SnipThreshold:          0.80,
		}
	default:
		return CompressionTier{
			Name:                   "tier-500k",
			SnipMaxToolResultChars: 6000,
			SnipThreshold:          0.85,
		}
	}
}

// applyTier mutates the compactor in place to match the given tier.
// Only fields the tier actually sets are written, so users with
// hand-tuned config.toml values for big knobs (Threshold for the
// LLM-backed Compact tier, MicrocompactMinChars, etc) keep them.
//
// Idempotent: calling applyTier with the same tier twice is a no-op.
func (c *Compactor) applyTier(t CompressionTier) {
	if t.Name == "" || t.Name == "unknown" {
		return
	}
	if t.SnipMaxToolResultChars > 0 {
		c.SnipMaxToolResultChars = t.SnipMaxToolResultChars
	}
	if t.SnipThreshold > 0 {
		c.SnipThreshold = t.SnipThreshold
	}
}

// ApplyWindowTier picks a tier from the supplied effective input cap
// (in tokens) and tightens the compactor's tool-history thresholds
// accordingly. Called once at Loop construction (right after
// MaxOutputTokens is set) so the rest of the compaction stack runs
// with already-tier-aware defaults.
//
// Public entry-point variant — exposes the auto-selection step
// without forcing callers to hold a CompressionTier value themselves.
func (c *Compactor) ApplyWindowTier(effectiveInputCapTokens int) {
	c.applyTier(tierForWindow(effectiveInputCapTokens))
}

// roundDownToTier rounds down to the nearest known tier boundary —
// helper for tests that want to assert "any value in this bucket
// resolves to this tier" without enumerating each token count.
func roundDownToTier(n int) int {
	for _, b := range []int{16_000, 32_000, 64_000, 128_000, 200_000} {
		if n <= b {
			return b
		}
	}
	if n <= 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return 500_000
	}
	return 500_000
}
