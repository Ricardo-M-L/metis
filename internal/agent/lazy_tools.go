// Package agent — lazy MCP tool schema (ToolSearch).
//
// claude-code's pattern: when an MCP-heavy session has 50+ tools whose
// JSON schemas balloon the prompt to 20K+ tokens, strip the schemas
// from the upfront tools list and expose a meta-tool ToolSearch. The
// model uses ToolSearch to fetch a specific tool's schema on demand,
// trading one extra round-trip for a much smaller per-iteration prompt.
//
// Trigger: lazyToolThreshold (configurable, default 20). When the total
// tool count is at or below this, full schemas are sent (current
// behavior). When over, MCP-prefixed (mcp__...) tools' schemas get
// replaced with a minimal placeholder, and ToolSearch is registered.
//
// Trade-off: lazy mode adds ~one tool-call round-trip the FIRST time
// the model uses each MCP tool. After that the schema is in the
// conversation history. Net win when MCP schemas dominate the prompt.
package agent

import (
	"encoding/json"
	"fmt"
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

// LazyToolThreshold is the tool-count cutoff above which MCP tool
// schemas are stripped and ToolSearch is exposed. 0 disables lazy
// mode entirely (current behavior, full schemas always).
//
// 20 is a conservative default — most metis sessions have ~12-15 core
// tools; lazy mode kicks in once a non-trivial number of MCP tools
// (≥5) are loaded. Tunable via Loop.LazyToolThreshold.
const LazyToolThresholdDefault = 20

// applyLazySchema rewrites specs in place when len(specs) > threshold:
//   - Strips InputSchema from any tool whose name starts with "mcp__"
//     (replacing it with a minimal placeholder so the model knows the
//     tool exists but must call ToolSearch for the parameter shape).
//   - Appends a synthetic ToolSearch entry that the runtime intercepts.
//
// Returns the (possibly augmented) spec slice. When lazy mode doesn't
// trigger, returns the input unchanged.
//
// Why we don't strip non-MCP tools too: the core tools (Read/Edit/Bash
// etc.) are always referenced; their schemas are small and well-known
// to the model. Lazy-loading those would add latency for zero benefit.
func applyLazySchema(specs []llm.ToolSpec, threshold int) []llm.ToolSpec {
	if threshold <= 0 || len(specs) <= threshold {
		return specs
	}
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
		return specs // threshold exceeded but all tools are core — no lazy savings to capture
	}
	out = append(out, toolSearchSpec(specs))
	return out
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
