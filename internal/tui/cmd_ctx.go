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

	"github.com/Ricardo-M-L/metis/internal/agent"
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

	cap := 0
	if loop.Provider != nil {
		cap = loop.Provider.MaxContextTokens()
	}
	if cap > 0 {
		b.WriteString(fmt.Sprintf("  context window:  %s tokens\n", fmtThousands(cap)))
	} else {
		b.WriteString("  context window:  unknown (provider didn't publish)\n")
	}

	c := loop.Compactor
	if c == nil {
		b.WriteString("\n  compactor:       not configured (disabled)\n")
		return b.String()
	}

	b.WriteString(fmt.Sprintf("  threshold:       %.2f (%d%%)\n",
		c.Threshold, int(c.Threshold*100)))
	if c.MinimumTokens > 0 {
		b.WriteString(fmt.Sprintf("  minimum tokens:  %s (floor — compact disabled below this)\n",
			fmtThousands(c.MinimumTokens)))
	}

	// Trigger point in absolute tokens. effectiveInputCap mirrors
	// agent.Compactor.effectiveInputCap() (unexported): cap minus the
	// reserved output budget, clamped at 0.
	effectiveCap := effectiveInputCap(c)
	trigger := int(float64(effectiveCap) * c.Threshold)
	b.WriteString(fmt.Sprintf("  trigger at:      %s tokens (effective cap %s × threshold)\n",
		fmtThousands(trigger), fmtThousands(effectiveCap)))

	// Live token estimate from the loop — same source the bottom status
	// bar reads.
	used := loop.EstimateContextTokens()
	b.WriteString(fmt.Sprintf("  current tokens:  %s\n", fmtThousands(used)))

	switch {
	case c.MinimumTokens > 0 && used < c.MinimumTokens:
		b.WriteString(fmt.Sprintf("  status:          BELOW MINIMUM FLOOR (%s < %s) — auto-compact gated\n",
			fmtThousands(used), fmtThousands(c.MinimumTokens)))
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

// effectiveInputCap mirrors agent.Compactor.effectiveInputCap()
// (unexported). Reserves min(MaxOutputTokens, agent.MaxReservedForSummary)
// from the context window so ShouldCompact accounts for the assistant's
// output budget; clamps at 0 / MaxContextTokens for edge cases.
func effectiveInputCap(c *agent.Compactor) int {
	reserved := c.MaxOutputTokens
	if reserved > agent.MaxReservedForSummary {
		reserved = agent.MaxReservedForSummary
	}
	cap := c.MaxContextTokens - reserved
	if cap <= 0 {
		return c.MaxContextTokens
	}
	return cap
}
