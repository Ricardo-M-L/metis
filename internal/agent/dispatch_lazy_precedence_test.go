package agent

// dispatch_lazy_precedence_test.go — pins the trigger-selection rules
// in dispatch.go::toolSpecs(). Mode comes from the
// ENABLE_TOOL_SEARCH env var; ContextWindow controls the auto-mode
// budget. This file is the single source of truth for "which mode
// fires when" given a registry + env combo.
//
// If a refactor reorders the cases in toolSpecs(), one of these
// tests breaks loudly.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// fatTool is a tools.Tool with a controllable schema bulk, used to dial
// in known token cost for trigger tests. Bulk lives in InputSchema
// (not Description), because dispatch.go::toolSpecs() runs every
// description through shortToolDesc which caps at 200 chars — any
// fatness in `desc` would be silently trimmed before the lazy-mode
// estimator sees it. Real-world MCP schema fatness is also schema-
// dominated (playwright's params, office-word's table options, etc.).
type fatTool struct {
	name      string
	desc      string
	schemaPad int // bytes of padding to inject into the schema's "description" field
}

func (f *fatTool) Name() string        { return f.name }
func (f *fatTool) Description() string { return f.desc }
func (f *fatTool) InputSchema() map[string]any {
	out := map[string]any{"type": "object"}
	if f.schemaPad > 0 {
		out["description"] = strings.Repeat("x", f.schemaPad)
	}
	return out
}
func (f *fatTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (f *fatTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (f *fatTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: ""}, nil
}

// hasToolSearchSpec reports whether the spec list ends with the
// synthetic ToolSearch entry — the marker that lazy mode actually
// fired. Checking by name (not just len change) avoids false positives.
func hasToolSearchSpec(specs []llm.ToolSpec) bool {
	for _, s := range specs {
		if s.Name == "ToolSearch" {
			return true
		}
	}
	return false
}

// TestDispatchToolSpecs_EnvFalseDisablesAll — ENABLE_TOOL_SEARCH=false
// is the explicit kill switch; no mode fires even with a fat MCP load
// that would otherwise trigger auto.
func TestDispatchToolSpecs_EnvFalseDisablesAll(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__a", schemaPad: 50000})
	l := &Loop{Registry: reg, ContextWindow: 16_000}
	if hasToolSearchSpec(l.toolSpecs()) {
		t.Errorf("ENABLE_TOOL_SEARCH=false must never trip")
	}
}

// TestDispatchToolSpecs_EnvTrueAlwaysFires — ENABLE_TOOL_SEARCH=true
// forces always-lazy. Even a single tiny MCP tool gets stripped, and
// ContextWindow is irrelevant.
func TestDispatchToolSpecs_EnvTrueAlwaysFires(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__tiny"})
	l := &Loop{Registry: reg, ContextWindow: 200_000}
	if !hasToolSearchSpec(l.toolSpecs()) {
		t.Errorf("ENABLE_TOOL_SEARCH=true must always fire even for tiny tools on huge windows")
	}
}

// TestDispatchToolSpecs_EnvTrueIgnoresUnknownWindow — always-mode
// works even when the provider didn't publish ContextWindow. This is
// the case where a custom or proxied LLM lookup table is missing —
// the user's explicit "true" still wins.
func TestDispatchToolSpecs_EnvTrueIgnoresUnknownWindow(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__tiny"})
	l := &Loop{Registry: reg, ContextWindow: 0}
	if !hasToolSearchSpec(l.toolSpecs()) {
		t.Errorf("always-mode must work without ContextWindow")
	}
}

// TestDispatchToolSpecs_AutoFiresOnSmallWindow — auto-10% (opt-in via
// explicit "auto" — empty default is now "always" since 2026-05-11).
// Fat MCP schema on 16k window (>10% of budget) trips the strip.
func TestDispatchToolSpecs_AutoFiresOnSmallWindow(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "auto")
	reg := tools.NewRegistry()
	// 4500-byte schema padding ≈ 1800 est tokens → > 10% of 16k = 1.6k.
	reg.Register(&fatTool{name: "mcp__playwright", schemaPad: 4500})
	l := &Loop{Registry: reg, ContextWindow: 16_000}
	if !hasToolSearchSpec(l.toolSpecs()) {
		t.Errorf("auto mode + 16k window + 1.8k MCP should fire")
	}
}

// TestDispatchToolSpecs_AutoSkipsOnBigWindow — same MCP load on a
// 200k-window model is well under 10%. Pins the "scales with the
// model" promise of auto mode (opted into via explicit "auto").
func TestDispatchToolSpecs_AutoSkipsOnBigWindow(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "auto")
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__playwright", schemaPad: 4500})
	l := &Loop{Registry: reg, ContextWindow: 200_000}
	if hasToolSearchSpec(l.toolSpecs()) {
		t.Errorf("auto mode + 200k window + 1.8k MCP should NOT fire")
	}
}

// TestDispatchToolSpecs_AutoCustomPercentage — auto:5 fires twice as
// aggressively. Same load that doesn't fire at 10% (200k window with
// 4500-byte schema, ≈1%) STILL doesn't fire at 5%. But raise the
// load 6× and 5% bites where 10% wouldn't.
func TestDispatchToolSpecs_AutoCustomPercentage(t *testing.T) {
	reg := tools.NewRegistry()
	// Aim for ~6% of 16k = ~960 tokens. 2400-byte schemaPad ≈ 960 tokens.
	reg.Register(&fatTool{name: "mcp__a", schemaPad: 2400})

	t.Run("auto:10 doesn't fire at 6%", func(t *testing.T) {
		t.Setenv("ENABLE_TOOL_SEARCH", "auto:10")
		l := &Loop{Registry: reg, ContextWindow: 16_000}
		if hasToolSearchSpec(l.toolSpecs()) {
			t.Errorf("6%% load < 10%% threshold should not fire")
		}
	})
	t.Run("auto:5 does fire at 6%", func(t *testing.T) {
		t.Setenv("ENABLE_TOOL_SEARCH", "auto:5")
		l := &Loop{Registry: reg, ContextWindow: 16_000}
		if !hasToolSearchSpec(l.toolSpecs()) {
			t.Errorf("6%% load > 5%% threshold should fire")
		}
	})
}

// TestDispatchToolSpecs_AutoZeroEqualsTrue — auto:0 and "true" must
// be observably equivalent (always lazy). The parser folds them to
// the same internal mode; this test pins that fold isn't lost in a
// future refactor.
func TestDispatchToolSpecs_AutoZeroEqualsTrue(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__tiny"})
	l := &Loop{Registry: reg, ContextWindow: 200_000}

	t.Setenv("ENABLE_TOOL_SEARCH", "auto:0")
	a := hasToolSearchSpec(l.toolSpecs())
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	b := hasToolSearchSpec(l.toolSpecs())
	if !a || !b {
		t.Errorf("auto:0 and true should both fire; got auto:0=%v true=%v", a, b)
	}
}

// TestDispatchToolSpecs_Auto100EqualsFalse — symmetric: auto:100 and
// "false" both mean "never lazy."
func TestDispatchToolSpecs_Auto100EqualsFalse(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__a", schemaPad: 50_000}) // huge — would fire any sane auto threshold
	l := &Loop{Registry: reg, ContextWindow: 16_000}

	t.Setenv("ENABLE_TOOL_SEARCH", "auto:100")
	a := hasToolSearchSpec(l.toolSpecs())
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	b := hasToolSearchSpec(l.toolSpecs())
	if a || b {
		t.Errorf("auto:100 and false should both NOT fire; got auto:100=%v false=%v", a, b)
	}
}

// TestDispatchToolSpecs_AutoUnknownWindowIsConservative — auto mode
// without a known ContextWindow can't compute a budget. We choose
// "no rewrite" over "guess" because a wrong strip breaks tool calls
// while a missed strip just costs tokens. Pin that contract.
func TestDispatchToolSpecs_AutoUnknownWindowIsConservative(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "auto")
	reg := tools.NewRegistry()
	reg.Register(&fatTool{name: "mcp__a", schemaPad: 50_000})
	l := &Loop{Registry: reg, ContextWindow: 0}
	if hasToolSearchSpec(l.toolSpecs()) {
		t.Errorf("auto mode with unknown ContextWindow must NOT strip")
	}
}
