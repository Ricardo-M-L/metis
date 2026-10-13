package tui

// cmd_ctx.go — `/ctx` slash command: print live compaction state so the
// user can answer "why hasn't auto-compact fired?" without me having
// to dig through config + screenshot math (user thread 2026-05-17,
// after screenshot 35/36 confusion: 99%+ on MiniMax-via-Anthropic
// cap=192k but threshold 0.8 trigger hadn't fired because the live
// iteration was mid-stream).
//
// Surface: provider name + cap, threshold + minimum, current tokens,
// distance to trigger, circuit-breaker state, and current iter index
// so the user can tell whether the trigger is just queued behind a
// long iter.

import (
	"fmt"
	"strings"
)

func cmdCtx(r *REPL, args string) string {
	if r == nil || r.Loop == nil {
		return "ctx: agent loop not running"
	}
	loop := r.Loop

	var b strings.Builder
	b.WriteString("Context state\n")
	b.WriteString("─────────────\n")

	// Provider's effective model — same string the request will carry.
	// Bottom-bar / top-bar copies can lag (screenshot 35 showed a stale
	// "deepseek-v4-pro" while the running provider was MiniMax via the
	// Anthropic gateway); loop.Model is authoritative.
	b.WriteString(fmt.Sprintf("  provider model:  %s\n", loop.Model))

	// Read the capability version installed on the Loop, not a provider cache
	// that may have refreshed since the compactor's request binding was built.
	capabilities, threshold, trigger := loop.ContextDetailsSnapshot()
	cap := capabilities.Window
	if cap > 0 {
		b.WriteString(fmt.Sprintf("  context window:  %s tokens (current working budget)\n", fmtThousands(cap)))
	} else {
		b.WriteString("  context window:  unknown (provider didn't publish)\n")
	}
	capacity := func(label string, value int) {
		if value > 0 {
			b.WriteString(fmt.Sprintf("  %-17s%s tokens\n", label+":", fmtThousands(value)))
		}
	}
	capacity("endpoint default", capabilities.DefaultWindow)
	capacity("client suggestion", capabilities.ProfileDefaultWindow)
	capacity("client catalog max", capabilities.ProfileMaxWindow)
	capacity("model capacity", capabilities.ModelWindow)
	capacity("model max input", capabilities.ModelMaxInputTokens)
	capacity("model max output", capabilities.ModelMaxOutputTokens)
	capacity("budget ceiling", capabilities.MaxWindow)
	capacity("max input", capabilities.MaxInputTokens)
	capacity("max output", capabilities.MaxOutputTokens)
	if capabilities.Source != "" {
		b.WriteString(fmt.Sprintf("  data source:     %s\n", capabilities.Source))
	}
	if capabilities.ModelSource != "" {
		b.WriteString(fmt.Sprintf("  model source:    %s\n", capabilities.ModelSource))
	}
	if capabilities.LimitSource != "" && capabilities.LimitSource != capabilities.Source {
		b.WriteString(fmt.Sprintf("  route source:    %s\n", capabilities.LimitSource))
	}
	if capabilities.BudgetSource != "" {
		b.WriteString(fmt.Sprintf("  budget source:   %s\n", capabilities.BudgetSource))
	}
	if capabilities.RequestedWindow > 0 && capabilities.RequestedWindow != cap {
		capacity("requested window", capabilities.RequestedWindow)
	}
	if capabilities.WindowKind == "input" {
		b.WriteString("  window kind:     input budget (total model capacity unknown)\n")
	}

	c := loop.Compactor
	if c == nil {
		b.WriteString("\n  compactor:       not configured (disabled)\n")
		return b.String()
	}

	b.WriteString(fmt.Sprintf("  threshold:       %.2f (%d%%)\n",
		threshold, int(threshold*100)))

	// Use the compactor's public boundary calculation rather than copying its
	// input-cap reservation logic here. That keeps this diagnostic identical to
	// ShouldCompact, including compatibility switches and future policy changes.
	minimum := c.MinimumTokens
	if trigger > 0 && minimum > trigger {
		minimum = trigger
	}
	if c.MinimumTokens > 0 {
		if minimum != c.MinimumTokens {
			b.WriteString(fmt.Sprintf("  minimum tokens:  %s (configured %s; clamped to trigger)\n",
				fmtThousands(minimum), fmtThousands(c.MinimumTokens)))
		} else {
			b.WriteString(fmt.Sprintf("  minimum tokens:  %s (floor — compact disabled below this)\n",
				fmtThousands(minimum)))
		}
	}

	if trigger > 0 {
		b.WriteString(fmt.Sprintf("  trigger at:      %s tokens (authoritative compactor boundary)\n",
			fmtThousands(trigger)))
	} else {
		b.WriteString("  trigger at:      disabled (threshold or input capacity unavailable)\n")
	}

	// Live token estimate from the loop — same source the bottom status
	// bar reads.
	used := loop.EstimateContextTokens()
	b.WriteString(fmt.Sprintf("  current tokens:  %s\n", fmtThousands(used)))

	switch {
	case trigger <= 0:
		b.WriteString("  status:          auto-compact disabled (no valid trigger boundary)\n")
	case minimum > 0 && used < minimum:
		b.WriteString(fmt.Sprintf("  status:          BELOW MINIMUM FLOOR (%s < %s) — auto-compact gated\n",
			fmtThousands(used), fmtThousands(minimum)))
	case used >= trigger:
		over := used - trigger
		b.WriteString(fmt.Sprintf("  status:          OVER trigger by %s tokens — will compact at next iter boundary\n",
			fmtThousands(over)))
	default:
		need := trigger - used
		b.WriteString(fmt.Sprintf("  status:          %s tokens until trigger\n",
			fmtThousands(need)))
	}

	if c.CircuitTripped() {
		b.WriteString("  circuit:         TRIPPED — auto-compact disabled until /clear or restart\n")
	}

	b.WriteString(fmt.Sprintf("  iter index:      %d (auto-compact runs at iter boundary, not mid-stream)\n",
		loop.IterIdx()))

	return b.String()
}
