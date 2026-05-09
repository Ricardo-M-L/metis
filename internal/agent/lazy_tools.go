// Package agent — lazy MCP tool schema (ToolSearch).
//
// claude-code's pattern: when an MCP-heavy session has 50+ tools whose
// JSON schemas balloon the prompt to 20K+ tokens, strip the schemas
// from the upfront tools list and expose a meta-tool ToolSearch. The
// model uses ToolSearch to fetch a specific tool's schema on demand,
// trading one extra round-trip for a much smaller per-iteration prompt.
//
// Mode is controlled by the `ENABLE_TOOL_SEARCH` environment variable
// (matches openclaude's `tst-auto` knob, src/utils/toolSearch.ts:172):
//
//	(unset)    → auto, fires when deferred tokens > 10% of context window
//	"auto"     → auto, 10%
//	"auto:N"   → auto, N% (1..99)
//	"auto:0"   → always lazy (equivalent to "true")
//	"auto:100" → never lazy (equivalent to "false")
//	"true"     → always lazy (deferred MCP tools always stripped)
//	"false"    → never lazy (full schemas always sent)
//
// Trade-off: lazy mode adds ~one tool-call round-trip the FIRST time
// the model uses each MCP tool. After that the schema is in the
// conversation history. Net win when MCP schemas dominate the prompt.
package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// handleToolSearch looks up the requested tool's full schema from the
// Loop's registry and returns it as a JSON-encoded tool_result. Used
// by dispatch.go's executeBatch when the model invokes the synthetic
// ToolSearch meta-tool (created by toolSearchSpec).
func handleToolSearch(l *Loop, b llm.ContentBlock) llm.ContentBlock {
	name, _ := b.ToolInput["name"].(string)
	if name == "" {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: b.ToolUseID,
			ToolResult: "error: ToolSearch requires {\"name\": \"<tool>\"}", IsError: true,
		}
	}
	t, ok := l.Registry.Get(name)
	if !ok {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: b.ToolUseID,
			ToolResult: fmt.Sprintf("error: tool %q not found", name), IsError: true,
		}
	}
	out := map[string]any{
		"name":         t.Name(),
		"description":  t.Description(),
		"input_schema": t.InputSchema(),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: b.ToolUseID,
			ToolResult: fmt.Sprintf("error: marshal failed: %v", err), IsError: true,
		}
	}
	return llm.ContentBlock{
		Type: "tool_result", ToolUseID: b.ToolUseID,
		ToolResult: string(raw),
	}
}

// LazyTokenPercentageDefault — the share of the model's context window
// that deferred (mcp__) tool definitions are allowed to occupy before
// auto-mode lazy kicks in. 10% matches openclaude's `tst-auto`
// default (toolSearch.ts:49). Lower → more aggressive, higher →
// more permissive.
//
// Why a percentage, not a fixed token count: a 16k-window model
// chokes on 6k of MCP schemas (37% of budget), but a 200k-window
// model wouldn't notice (3%). One static threshold can't satisfy
// both — a percentage tracks the model's actual budget.
const LazyTokenPercentageDefault = 10

// LazyMode is the user-selected lazy-tool-loading mode parsed from
// ENABLE_TOOL_SEARCH. Mirrors openclaude's `ToolSearchMode` tri-state.
type LazyMode int

const (
	// LazyModeStandard — never strip schemas. The full tools[] array
	// (including MCP) is sent every turn. Use when the model is
	// confused by deferred tools or you're debugging tool selection.
	LazyModeStandard LazyMode = iota

	// LazyModeAuto — strip schemas when deferred (mcp__) tools'
	// estimated token cost exceeds Percentage% of the context window.
	// The default. Adapts to the model's window automatically.
	LazyModeAuto

	// LazyModeAlways — always strip mcp__ schemas, regardless of
	// budget. Useful for very small-window providers where even
	// modest MCP loads matter, or for matching claude-code's
	// "always-defer" behavior on Anthropic models.
	LazyModeAlways
)

// parseEnableToolSearch resolves the ENABLE_TOOL_SEARCH env value into
// a (mode, percentage) pair. Empty / unrecognized values fall back to
// the default (auto, LazyTokenPercentageDefault). Match table:
//
//	value         mode      percentage    notes
//	(empty)       Auto      10            default
//	"auto"        Auto      10
//	"auto:N"      Auto      N             1 ≤ N ≤ 99
//	"auto:0"      Always    0             same as "true"
//	"auto:100"    Standard  0             same as "false"
//	"true"/"1"/"yes"/"on"    Always    0
//	"false"/"0"/"no"/"off"   Standard  0
//	other         Auto      10            silent fallback
//
// Lower-cased and trimmed before matching so users can write
// `ENABLE_TOOL_SEARCH=Auto:25` or `ENABLE_TOOL_SEARCH= true ` and have
// it work. Returns the percentage as 0 for non-Auto modes (irrelevant).
func parseEnableToolSearch(value string) (LazyMode, int) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == "auto" {
		return LazyModeAuto, LazyTokenPercentageDefault
	}
	if strings.HasPrefix(v, "auto:") {
		n, err := strconv.Atoi(strings.TrimPrefix(v, "auto:"))
		if err != nil {
			return LazyModeAuto, LazyTokenPercentageDefault
		}
		if n <= 0 {
			return LazyModeAlways, 0
		}
		if n >= 100 {
			return LazyModeStandard, 0
		}
		return LazyModeAuto, n
	}
	switch v {
	case "true", "1", "yes", "on":
		return LazyModeAlways, 0
	case "false", "0", "no", "off":
		return LazyModeStandard, 0
	}
	return LazyModeAuto, LazyTokenPercentageDefault
}

// charsPerToken — rough char-to-token ratio for ToolSpec JSON
// (name + description + schema). 2.5 is openclaude's fallback
// constant (toolSearch.ts:99) chosen for JSON-heavy text where
// punctuation density is high; the more common "4 chars/token"
// rule of thumb under-counts tool schemas by ~40%.
const charsPerToken = 2.5

// stripAndAppendToolSearch performs the actual lazy-mode rewrite,
// independent of the trigger logic. Pulled out so both the legacy
// count-based trigger (applyLazySchema) and the token-budget trigger
// (applyLazySchemaByTokens) share one implementation.
//
// The rewrite:
//   - Strips InputSchema from any tool whose name starts with "mcp__"
//     (replacing it with a minimal placeholder so the model knows the
//     tool exists but must call ToolSearch for the parameter shape).
//   - Appends a synthetic ToolSearch entry that the runtime intercepts.
//
// When the input contains no mcp__ tools, returns it unchanged — a
// ToolSearch entry pointing at zero deferred tools would just be noise.
//
// Why we don't strip non-MCP tools too: the core tools (Read/Edit/Bash
// etc.) are always referenced; their schemas are small and well-known
// to the model. Lazy-loading those would add latency for zero benefit.
func stripAndAppendToolSearch(specs []llm.ToolSpec) []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(specs)+1)
	hasMCP := false
	for _, s := range specs {
		if strings.HasPrefix(s.Name, "mcp__") {
			hasMCP = true
			out = append(out, llm.ToolSpec{
				Name:        s.Name,
				Description: s.Description + "  [schema lazy — call ToolSearch to fetch parameters before invoking]",
				InputSchema: lazyPlaceholderSchema(),
			})
			continue
		}
		out = append(out, s)
	}
	if !hasMCP {
		return specs
	}
	out = append(out, toolSearchSpec(specs))
	return out
}

// applyLazySchemaByTokens triggers lazy mode when the deferred (mcp__)
// tools' total token estimate exceeds `contextWindow * percentage / 100`.
//
// Why this is a better default than count-based: a single big-schema
// MCP tool (e.g. playwright with ~5k tokens of schema) on a 16k-window
// model is a runaway, but five small MCP tools (~50 tokens each) on a
// 200k-window model are noise. Count-based triggers miss the first and
// over-fire on the second — token-based gets both right.
//
// percentage values:
//   - ≤ 0  → disabled (no-op)
//   - ≥ 100 → disabled (allows MCP schemas to occupy 100% of budget)
//   - default LazyTokenPercentageDefault (10) when caller passes 0
//
// contextWindow ≤ 0 → no-op (caller should fall back to the legacy
// count-based path).
func applyLazySchemaByTokens(specs []llm.ToolSpec, contextWindow, percentage int) []llm.ToolSpec {
	if contextWindow <= 0 || percentage <= 0 || percentage >= 100 {
		return specs
	}
	deferredTokens := 0
	for _, s := range specs {
		if strings.HasPrefix(s.Name, "mcp__") {
			deferredTokens += estimateSpecTokens(s)
		}
	}
	budget := contextWindow * percentage / 100
	if deferredTokens < budget {
		return specs
	}
	return stripAndAppendToolSearch(specs)
}

// estimateSpecTokens — cheap token estimate for a single ToolSpec, used
// only for the lazy-mode trigger decision. Sums name + description +
// JSON-marshalled schema length, divides by charsPerToken (2.5).
//
// Not exact — a real tokenizer would be ~10× slower per call and
// requires loading a vocab. The error band is ±20%, which is fine for
// a "should we strip schemas" branch (the budget itself is heuristic).
func estimateSpecTokens(s llm.ToolSpec) int {
	n := len(s.Name) + len(s.Description)
	if raw, err := json.Marshal(s.InputSchema); err == nil {
		n += len(raw)
	}
	// Framing overhead for the {"name":..., "description":..., "input_schema":...} wrapper.
	n += 32
	return int(float64(n) / charsPerToken)
}

// lazyPlaceholderSchema is the minimal "any object" schema we leave
// behind when stripping a real MCP schema. The model cannot call the
// tool successfully with this (params will be unvalidated), which is
// the point — it must go via ToolSearch first to learn what params
// the tool actually takes.
func lazyPlaceholderSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          "schema deferred — call ToolSearch first",
		"additionalProperties": true,
	}
}

// toolSearchSpec is the meta-tool definition the model uses to fetch a
// real tool's schema by name. Returns the schema as a JSON-encoded
// string in the tool_result, which the model parses on the next
// iteration. The runtime intercept (in dispatch.go) handles the
// actual lookup against the registry.
//
// We embed a list of all available tool names directly in the
// description so the model doesn't have to guess names — gives it
// "what's available" without paying for the full schemas.
func toolSearchSpec(allSpecs []llm.ToolSpec) llm.ToolSpec {
	var names []string
	for _, s := range allSpecs {
		names = append(names, s.Name)
	}
	desc := "Returns the JSON schema for a registered tool by name. Call this BEFORE invoking any tool whose schema is marked '[schema lazy]'. Available tool names:\n  " +
		strings.Join(names, ", ")
	return llm.ToolSpec{
		Name:        "ToolSearch",
		Description: desc,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "exact tool name to look up (e.g. mcp__filesystem__read)",
				},
			},
			"required": []string{"name"},
		},
	}
}
