package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestApplyLazySchema_BelowThreshold — when total tools <= threshold,
// specs pass through unchanged. The lazy machinery should be invisible
// in the common case.
func TestApplyLazySchema_BelowThreshold(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": "..."}}},
		{Name: "mcp__fs__read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": "..."}}},
	}
	out := applyLazySchema(specs, 20)
	if len(out) != len(specs) {
		t.Fatalf("below threshold: spec count must not change; got %d want %d", len(out), len(specs))
	}
	for i, s := range out {
		if _, ok := s.InputSchema["properties"]; !ok {
			t.Errorf("spec[%d] (%s) lost its real schema; got %+v", i, s.Name, s.InputSchema)
		}
	}
}

// TestApplyLazySchema_Disabled — threshold 0 disables lazy mode entirely.
func TestApplyLazySchema_Disabled(t *testing.T) {
	specs := make([]llm.ToolSpec, 50)
	for i := range specs {
		specs[i] = llm.ToolSpec{Name: "mcp__test__t" + string(rune('a'+i%26)), InputSchema: map[string]any{"x": "y"}}
	}
	out := applyLazySchema(specs, 0)
	if len(out) != len(specs) {
		t.Errorf("threshold=0 should be no-op; got %d != %d", len(out), len(specs))
	}
}

// TestApplyLazySchema_StripsMCPSchemas — over-threshold path: mcp__-prefixed
// schemas get replaced with the lazy placeholder, core tools stay intact,
// ToolSearch is appended.
func TestApplyLazySchema_StripsMCPSchemas(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"properties": "real"}},
		{Name: "Bash", InputSchema: map[string]any{"properties": "real"}},
		{Name: "mcp__fs__read", InputSchema: map[string]any{"properties": "real-mcp-1"}},
		{Name: "mcp__fs__write", InputSchema: map[string]any{"properties": "real-mcp-2"}},
		{Name: "mcp__http__get", InputSchema: map[string]any{"properties": "real-mcp-3"}},
	}
	out := applyLazySchema(specs, 4) // 5 > 4 → trigger

	// Expect 5 originals + 1 ToolSearch = 6 total.
	if len(out) != 6 {
		t.Fatalf("expected 6 specs (5 + ToolSearch); got %d", len(out))
	}
	// Core tools keep their real schema.
	for _, s := range out[:2] {
		if s.InputSchema["properties"] != "real" {
			t.Errorf("core tool %s schema was stripped; got %+v", s.Name, s.InputSchema)
		}
	}
	// MCP tools have placeholder schema.
	for _, s := range out[2:5] {
		if s.InputSchema["additionalProperties"] != true {
			t.Errorf("mcp tool %s schema was NOT stripped; got %+v", s.Name, s.InputSchema)
		}
		if !strings.Contains(s.Description, "schema lazy") {
			t.Errorf("mcp tool %s description missing lazy hint; got %q", s.Name, s.Description)
		}
	}
	// Last entry is ToolSearch.
	if out[5].Name != "ToolSearch" {
		t.Errorf("expected ToolSearch as last entry; got %q", out[5].Name)
	}
	// ToolSearch description lists all tool names so the model knows what's available.
	for _, s := range specs {
		if !strings.Contains(out[5].Description, s.Name) {
			t.Errorf("ToolSearch description missing tool name %q", s.Name)
		}
	}
}

// TestApplyLazySchema_NoMCPNoChange — over-threshold but no MCP tools
// means there's nothing to strip; we don't append ToolSearch since it
// would be useless.
func TestApplyLazySchema_NoMCPNoChange(t *testing.T) {
	specs := make([]llm.ToolSpec, 25)
	for i := range specs {
		specs[i] = llm.ToolSpec{Name: "core_" + string(rune('a'+i%26)), InputSchema: map[string]any{"x": "y"}}
	}
	out := applyLazySchema(specs, 20)
	if len(out) != len(specs) {
		t.Errorf("no MCP tools → no ToolSearch should be appended; got %d != %d", len(out), len(specs))
	}
	for i, s := range out {
		if s.Name != specs[i].Name {
			t.Errorf("specs reordered or mutated at index %d", i)
		}
	}
}
