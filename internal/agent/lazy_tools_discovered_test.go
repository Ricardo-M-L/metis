package agent

// lazy_tools_discovered_test.go — locks the cross-compaction stability
// behavior added in P6. Three goals:
//
//	1. handleSelectQuery → markMCPDiscovered: once a schema is handed
//	   to the model, the Loop remembers it.
//	2. stripAndAppendToolSearchWithPreserve: preserved tools keep
//	   their full schemas even when the rest get stripped.
//	3. rebuildDiscoveredMCPFromMessages: a resumed session populates
//	   the discovered set from prior ToolSearch tool_result blocks.

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestMarkMCPDiscovered_RecordsName(t *testing.T) {
	l := &Loop{}
	l.markMCPDiscovered("mcp__fs__read")
	l.markMCPDiscovered("Read") // non-MCP — should be ignored
	got := l.snapshotDiscoveredMCP()
	if len(got) != 1 || !got["mcp__fs__read"] {
		t.Errorf("expected only mcp__fs__read; got %v", got)
	}
}

// TestHandleSelectQuery_MarksDiscovered — the end-to-end path: a
// successful select: invocation must populate Loop.discoveredMCP so
// the next toolSpecs() build can preserve the schema.
func TestHandleSelectQuery_MarksDiscovered(t *testing.T) {
	l := newLoopWithTools(
		mcpFake("mcp__alpha", "alpha"),
		mcpFake("mcp__beta", "beta"),
	)
	_, isErr := invokeSearch(t, l, map[string]any{"query": "select:mcp__alpha,mcp__beta"})
	if isErr {
		t.Fatalf("setup: select should succeed")
	}
	got := l.snapshotDiscoveredMCP()
	if !got["mcp__alpha"] || !got["mcp__beta"] {
		t.Errorf("both names should be marked discovered; got %v", got)
	}
}

// TestStripAndAppendToolSearchWithPreserve_KeepsDiscovered — the
// rewriter must NOT strip schemas of preserved tools, but still strips
// + appends ToolSearch for any non-preserved mcp__ tools so the model
// can fetch them on demand.
func TestStripAndAppendToolSearchWithPreserve_KeepsDiscovered(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"core": true}},
		{Name: "mcp__fs__read", InputSchema: map[string]any{"path": "real"}},
		{Name: "mcp__fs__write", InputSchema: map[string]any{"path": "real"}},
		{Name: "mcp__http__get", InputSchema: map[string]any{"url": "real"}},
	}
	preserve := map[string]bool{"mcp__fs__read": true}

	out := stripAndAppendToolSearchWithPreserve(specs, preserve)

	if len(out) != 5 {
		t.Fatalf("expected 4 originals + ToolSearch = 5; got %d", len(out))
	}
	// Read unchanged
	if out[0].InputSchema["core"] != true {
		t.Errorf("core Read schema must pass through; got %+v", out[0].InputSchema)
	}
	// mcp__fs__read preserved — schema kept
	if out[1].InputSchema["path"] != "real" {
		t.Errorf("preserved mcp__fs__read schema should be intact; got %+v", out[1].InputSchema)
	}
	// mcp__fs__write stripped — placeholder
	if out[2].InputSchema["additionalProperties"] != true {
		t.Errorf("non-preserved mcp__fs__write should be stripped; got %+v", out[2].InputSchema)
	}
	// mcp__http__get stripped — placeholder
	if out[3].InputSchema["additionalProperties"] != true {
		t.Errorf("non-preserved mcp__http__get should be stripped; got %+v", out[3].InputSchema)
	}
	if out[4].Name != "ToolSearch" {
		t.Errorf("ToolSearch should be appended; got last entry %q", out[4].Name)
	}
}

// TestStripAndAppendToolSearchWithPreserve_AllPreserved_NoMetaTool —
// when every MCP tool is already discovered, the synthetic ToolSearch
// entry would be pointless. Don't append it.
func TestStripAndAppendToolSearchWithPreserve_AllPreserved_NoMetaTool(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"core": true}},
		{Name: "mcp__a", InputSchema: map[string]any{"real": "schema"}},
		{Name: "mcp__b", InputSchema: map[string]any{"real": "schema"}},
	}
	preserve := map[string]bool{"mcp__a": true, "mcp__b": true}

	out := stripAndAppendToolSearchWithPreserve(specs, preserve)

	if len(out) != 3 {
		t.Fatalf("all-preserved should pass through unchanged length; got %d (%+v)", len(out), out)
	}
	for _, s := range out {
		if s.Name == "ToolSearch" {
			t.Errorf("ToolSearch should NOT be appended when nothing is stripped")
		}
	}
}

// TestRebuildDiscoveredMCPFromMessages_RoundTrip — the hydration
// path. A Messages slice containing a prior assistant ToolSearch
// invocation + its successful user tool_result should populate the
// set on first read.
func TestRebuildDiscoveredMCPFromMessages_RoundTrip(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "use the fs tool"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{
				Type:      "tool_use",
				ToolUseID: "tu-1",
				ToolName:  "ToolSearch",
				ToolInput: map[string]any{"query": "select:mcp__fs__read"},
			},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{
				Type:       "tool_result",
				ToolUseID:  "tu-1",
				ToolResult: `{"matches":[{"name":"mcp__fs__read","description":"reads","input_schema":{"type":"object"}}]}`,
			},
		}},
	}
	set := make(map[string]bool)
	rebuildDiscoveredMCPFromMessages(set, msgs)
	if !set["mcp__fs__read"] {
		t.Errorf("expected mcp__fs__read in set; got %v", set)
	}
}

// TestRebuildDiscoveredMCPFromMessages_SkipsErrorResults — IsError
// results never delivered a schema, so they shouldn't populate the
// discovered set. Otherwise a transient error would falsely mark a
// tool as discovered and the next turn would assume the model has
// the schema when it doesn't.
func TestRebuildDiscoveredMCPFromMessages_SkipsErrorResults(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{
				Type:      "tool_use",
				ToolUseID: "tu-1",
				ToolName:  "ToolSearch",
				ToolInput: map[string]any{"query": "select:mcp__a"},
			},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{
				Type:       "tool_result",
				ToolUseID:  "tu-1",
				ToolResult: `error: tool "mcp__a" not found`,
				IsError:    true,
			},
		}},
	}
	set := make(map[string]bool)
	rebuildDiscoveredMCPFromMessages(set, msgs)
	if len(set) != 0 {
		t.Errorf("error result should NOT populate discovered set; got %v", set)
	}
}

// TestRebuildDiscoveredMCPFromMessages_IgnoresNonToolSearch — only
// ToolSearch tool_use blocks populate the set. A regular `mcp__foo`
// tool_use that returned a result doesn't count (we want schemas
// the model SAW via ToolSearch, not schemas it already had).
func TestRebuildDiscoveredMCPFromMessages_IgnoresNonToolSearch(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{
				Type:      "tool_use",
				ToolUseID: "tu-1",
				ToolName:  "mcp__fs__read", // direct invocation, not ToolSearch
				ToolInput: map[string]any{"path": "/etc/hosts"},
			},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{
				Type:       "tool_result",
				ToolUseID:  "tu-1",
				ToolResult: `{"matches":[{"name":"mcp__fs__read"}]}`, // shaped like our payload, but wrong tool
			},
		}},
	}
	set := make(map[string]bool)
	rebuildDiscoveredMCPFromMessages(set, msgs)
	if len(set) != 0 {
		t.Errorf("non-ToolSearch tool_use must not populate set; got %v", set)
	}
}

// TestEnsureDiscoveredHydrated_RunsOnce — lazy hydration must not
// re-walk message history on every snapshot call. Verify via the
// hydrated flag rather than direct mutation count (which would
// require adding test hooks).
func TestEnsureDiscoveredHydrated_RunsOnce(t *testing.T) {
	l := &Loop{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUseID: "tu-1", ToolName: "ToolSearch"},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Type: "tool_result", ToolUseID: "tu-1", ToolResult: `{"matches":[{"name":"mcp__rehydrated","input_schema":{"type":"object"}}]}`},
			}},
		},
	}
	l.snapshotDiscoveredMCP() // first call hydrates
	if !l.discoveredMCPHydrated {
		t.Fatalf("hydrated flag should be true after first snapshot")
	}
	// Add a new ToolSearch result AFTER hydration; it should NOT be
	// picked up by subsequent snapshots — hydration runs once.
	l.Messages = append(l.Messages,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "tu-2", ToolName: "ToolSearch"},
		}},
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu-2", ToolResult: `{"matches":[{"name":"mcp__post_hydration","input_schema":{"type":"object"}}]}`},
		}},
	)
	got := l.snapshotDiscoveredMCP()
	if got["mcp__post_hydration"] {
		t.Errorf("post-hydration messages should NOT auto-populate (markMCPDiscovered is the live path); got %v", got)
	}
	if !got["mcp__rehydrated"] {
		t.Errorf("initially-hydrated entry should remain; got %v", got)
	}
}

func TestRebuildDiscoveredMCPFromMessages_KeywordMatchDoesNotHydrate(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "keyword-1", ToolName: "ToolSearch"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "keyword-1", ToolResult: `{"matches":[{"name":"mcp__docs__query","description":"search docs"}]}`},
		}},
	}
	set := make(map[string]bool)
	rebuildDiscoveredMCPFromMessages(set, msgs)
	if len(set) != 0 {
		t.Fatalf("keyword-only matches must not unlock schemas: %v", set)
	}
}

// TestDispatchToolSpecs_PreservesDiscovered — end-to-end: a Loop with
// a registered MCP tool, after one ToolSearch invocation, produces a
// toolSpecs() output where that tool's schema is intact (not stripped).
func TestDispatchToolSpecs_PreservesDiscovered(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true") // force always-defer mode

	reg := tools.NewRegistry()
	reg.Register(fakeMCPTool{
		name: "mcp__alpha", description: "alpha",
		schema: map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}},
	})
	reg.Register(fakeMCPTool{
		name: "mcp__beta", description: "beta",
		schema: map[string]any{"properties": map[string]any{"y": map[string]any{"type": "number"}}},
	})
	l := &Loop{Registry: reg, ContextWindow: 192_000}

	// First call: both tools stripped.
	specs := l.toolSpecs()
	for _, s := range specs {
		if s.Name == "mcp__alpha" || s.Name == "mcp__beta" {
			if s.InputSchema["additionalProperties"] != true {
				t.Errorf("expected %s stripped initially; got %+v", s.Name, s.InputSchema)
			}
		}
	}

	// Simulate the model invoking ToolSearch to discover mcp__alpha.
	l.markMCPDiscovered("mcp__alpha")
	// Force re-hydration is not needed — markMCPDiscovered writes to
	// the live set, and ensureDiscoveredHydrated short-circuits.
	specs = l.toolSpecs()

	// alpha should now be intact, beta should still be stripped.
	hasMCPAlpha := false
	for _, s := range specs {
		if s.Name == "mcp__alpha" {
			hasMCPAlpha = true
			props, _ := s.InputSchema["properties"].(map[string]any)
			if _, ok := props["x"]; !ok {
				t.Errorf("mcp__alpha schema should be restored; got %+v", s.InputSchema)
			}
		}
		if s.Name == "mcp__beta" {
			if s.InputSchema["additionalProperties"] != true {
				t.Errorf("mcp__beta should still be stripped; got %+v", s.InputSchema)
			}
		}
	}
	if !hasMCPAlpha {
		t.Errorf("mcp__alpha should still appear in specs; got names %v", specNames(specs))
	}
	// ToolSearch is still appended because beta remains stripped.
	if !containsName(specs, "ToolSearch") {
		t.Errorf("ToolSearch should be appended when any tool remains deferred")
	}
}

func specNames(specs []llm.ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func containsName(specs []llm.ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// TestParseMCPNamesFromResult_HandlesMalformed — the JSON parser
// shouldn't crash on garbage. Models occasionally produce malformed
// payloads (or the body gets truncated by compaction); silently
// skipping malformed bodies is the right behaviour.
func TestParseMCPNamesFromResult_HandlesMalformed(t *testing.T) {
	cases := []string{
		"",
		"not json at all",
		`{"unexpected": "shape"}`,
		`{"matches": "not an array"}`,
		`{"matches": [{"no_name_field": "oops"}]}`,
	}
	for _, body := range cases {
		set := make(map[string]bool)
		parseMCPNamesFromResult(set, body)
		if len(set) != 0 {
			t.Errorf("malformed body %q should yield empty set; got %v", body, set)
		}
	}
}

// TestParseMCPNamesFromResult_OnlyMCPPrefixed — non-mcp__ names in a
// "matches" array (e.g. a future builtin-listing variant) should be
// ignored; only mcp__ names track schema deferral.
func TestParseMCPNamesFromResult_OnlyMCPPrefixed(t *testing.T) {
	body := `{"matches":[
		{"name":"mcp__fs__read","input_schema":{}},
		{"name":"Read"},
		{"name":"mcp__http__get","input_schema":{}},
		{"name":"Bash"}
	]}`
	set := make(map[string]bool)
	parseMCPNamesFromResult(set, body)
	if !set["mcp__fs__read"] || !set["mcp__http__get"] {
		t.Errorf("mcp__ names missing; got %v", set)
	}
	if set["Read"] || set["Bash"] {
		t.Errorf("non-mcp__ names leaked into set; got %v", set)
	}
	// Defensive: total size should be exactly 2.
	if len(set) != 2 {
		t.Errorf("expected exactly 2 entries; got %d (%v)", len(set), set)
	}
}

// Sanity: the strings package is used elsewhere — keep this here so
// goimports doesn't strip it if a future edit removes the only direct
// reference.
var _ = strings.HasPrefix
