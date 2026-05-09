package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// fatMCPSpec returns an MCP-prefixed spec with `descLen` chars in its
// description, used to dial in a known token cost. With
// charsPerToken=2.5, descLen=2500 → ~1000 estimated tokens.
//
// Note: in production, dispatch.go::toolSpecs() runs descriptions
// through shortToolDesc (200-char cap), so realistic fatness comes
// from InputSchema. These tests call applyLazySchemaByTokens directly
// (no shortToolDesc layer), so description-based padding is fine here.
func fatMCPSpec(name string, descLen int) llm.ToolSpec {
	desc := strings.Repeat("x", descLen)
	return llm.ToolSpec{
		Name: name, Description: desc,
		InputSchema: map[string]any{"type": "object"},
	}
}

// TestStripAndAppendToolSearch_StripsMCPSchemas — the core rewriter:
// every mcp__-prefixed tool gets its schema replaced with the
// placeholder, every other tool stays intact, and a synthetic
// ToolSearch entry is appended.
func TestStripAndAppendToolSearch_StripsMCPSchemas(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"properties": "real"}},
		{Name: "Bash", InputSchema: map[string]any{"properties": "real"}},
		{Name: "mcp__fs__read", InputSchema: map[string]any{"properties": "real-mcp-1"}},
		{Name: "mcp__fs__write", InputSchema: map[string]any{"properties": "real-mcp-2"}},
		{Name: "mcp__http__get", InputSchema: map[string]any{"properties": "real-mcp-3"}},
	}
	out := stripAndAppendToolSearch(specs)

	if len(out) != 6 {
		t.Fatalf("expected 6 specs (5 + ToolSearch); got %d", len(out))
	}
	for _, s := range out[:2] {
		if s.InputSchema["properties"] != "real" {
			t.Errorf("core tool %s schema was stripped; got %+v", s.Name, s.InputSchema)
		}
	}
	for _, s := range out[2:5] {
		if s.InputSchema["additionalProperties"] != true {
			t.Errorf("mcp tool %s schema was NOT stripped; got %+v", s.Name, s.InputSchema)
		}
		if !strings.Contains(s.Description, "schema lazy") {
			t.Errorf("mcp tool %s description missing lazy hint; got %q", s.Name, s.Description)
		}
	}
	if out[5].Name != "ToolSearch" {
		t.Errorf("expected ToolSearch as last entry; got %q", out[5].Name)
	}
	for _, s := range specs {
		if !strings.Contains(out[5].Description, s.Name) {
			t.Errorf("ToolSearch description missing tool name %q", s.Name)
		}
	}
}

// TestStripAndAppendToolSearch_NoMCPNoChange — over-trigger but no MCP
// tools means there's nothing to strip; we don't append ToolSearch
// since it would be useless.
func TestStripAndAppendToolSearch_NoMCPNoChange(t *testing.T) {
	specs := make([]llm.ToolSpec, 25)
	for i := range specs {
		specs[i] = llm.ToolSpec{Name: "core_" + string(rune('a'+i%26)), InputSchema: map[string]any{"x": "y"}}
	}
	out := stripAndAppendToolSearch(specs)
	if len(out) != len(specs) {
		t.Errorf("no MCP tools → no ToolSearch should be appended; got %d != %d", len(out), len(specs))
	}
	for i, s := range out {
		if s.Name != specs[i].Name {
			t.Errorf("specs reordered or mutated at index %d", i)
		}
	}
}

// TestApplyLazySchemaByTokens_SmallWindowFires — the painful case. A
// 16k window with ~2k tokens of MCP schema (12.5% of budget) MUST
// trigger lazy mode under the default 10% percentage.
func TestApplyLazySchemaByTokens_SmallWindowFires(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		fatMCPSpec("mcp__playwright__navigate", 5000), // ~2000 est tokens
	}
	out := applyLazySchemaByTokens(specs, 16_000, 10)
	if len(out) != 3 {
		t.Fatalf("16k+12.5%% should fire; got %d specs (want 3)", len(out))
	}
	if out[2].Name != "ToolSearch" {
		t.Errorf("expected ToolSearch as last entry; got %q", out[2].Name)
	}
	if out[1].InputSchema["additionalProperties"] != true {
		t.Errorf("MCP schema not stripped; got %+v", out[1].InputSchema)
	}
}

// TestApplyLazySchemaByTokens_BigWindowSkips — symmetric case: same
// MCP load on a 200k window is only 1% of budget, far under 10%.
// Must NOT fire.
func TestApplyLazySchemaByTokens_BigWindowSkips(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		fatMCPSpec("mcp__playwright__navigate", 5000),
	}
	out := applyLazySchemaByTokens(specs, 200_000, 10)
	if len(out) != len(specs) {
		t.Errorf("200k+1%% should pass through; got %d (want %d)", len(out), len(specs))
	}
	if out[1].InputSchema["additionalProperties"] == true {
		t.Errorf("MCP schema should NOT be stripped under generous budget")
	}
}

// TestApplyLazySchemaByTokens_PercentageDisabled — pct ≤ 0 or ≥ 100
// must be a no-op. Both ends mean "trigger never fires" under auto.
func TestApplyLazySchemaByTokens_PercentageDisabled(t *testing.T) {
	specs := []llm.ToolSpec{
		fatMCPSpec("mcp__a", 10000),
		fatMCPSpec("mcp__b", 10000),
		fatMCPSpec("mcp__c", 10000),
	}
	for _, pct := range []int{-1, 0, 100, 200} {
		out := applyLazySchemaByTokens(specs, 16_000, pct)
		if len(out) != len(specs) {
			t.Errorf("pct=%d should disable; got %d (want %d)", pct, len(out), len(specs))
		}
	}
}

// TestApplyLazySchemaByTokens_UnknownWindow — contextWindow ≤ 0 is
// "provider didn't publish a window." Pass through unchanged; the
// dispatch.go caller decides what to do (currently: also pass through).
func TestApplyLazySchemaByTokens_UnknownWindow(t *testing.T) {
	specs := []llm.ToolSpec{fatMCPSpec("mcp__a", 50000)}
	out := applyLazySchemaByTokens(specs, 0, 10)
	if len(out) != 1 {
		t.Errorf("unknown window should pass through; got %d", len(out))
	}
}

// TestApplyLazySchemaByTokens_UsesAggregateNotMax — three small MCP
// tools (~700 tokens each) on a 16k window should fire collectively
// (~2.1k > 1.6k), even though no single one would. Pins that the
// trigger is the SUM, not the max — otherwise users could paper
// over the issue by splitting one big tool into many small ones.
func TestApplyLazySchemaByTokens_UsesAggregateNotMax(t *testing.T) {
	specs := []llm.ToolSpec{
		fatMCPSpec("mcp__a", 1750), // ~700 tokens each
		fatMCPSpec("mcp__b", 1750),
		fatMCPSpec("mcp__c", 1750),
	}
	out := applyLazySchemaByTokens(specs, 16_000, 10)
	hasToolSearch := false
	for _, s := range out {
		if s.Name == "ToolSearch" {
			hasToolSearch = true
		}
	}
	if !hasToolSearch {
		t.Errorf("aggregate >budget should fire even when no single tool exceeds; got specs=%d, ToolSearch=%v",
			len(out), hasToolSearch)
	}
}

// TestEstimateSpecTokens_RoughlyMatchesCharsPerToken — sanity check
// the estimator: a 250-char description should land near 100 tokens
// (250/2.5), within the framing-overhead band. Catches refactors that
// accidentally use 4 chars/token (under-counts ~40%) or skip schema.
func TestEstimateSpecTokens_RoughlyMatchesCharsPerToken(t *testing.T) {
	spec := llm.ToolSpec{
		Name:        "mcp__test__t",
		Description: strings.Repeat("x", 250),
		InputSchema: map[string]any{"type": "object"},
	}
	got := estimateSpecTokens(spec)
	want := 100
	if got < int(float64(want)*0.75) || got > int(float64(want)*1.5) {
		t.Errorf("estimateSpecTokens(250-char desc) = %d, want ~%d (±25-50%%)", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// parseEnableToolSearch — the env-var match table. Every documented
// form needs a row in this table; if the parser drifts, the affected
// row breaks immediately. Mirrors openclaude's getToolSearchMode test
// matrix (src/utils/toolSearch.ts:172).
// ─────────────────────────────────────────────────────────────────────

func TestParseEnableToolSearch(t *testing.T) {
	cases := []struct {
		in       string
		wantMode LazyMode
		wantPct  int
	}{
		// default + explicit auto
		{"", LazyModeAuto, 10},
		{"auto", LazyModeAuto, 10},
		{"AUTO", LazyModeAuto, 10}, // case-insensitive
		{" auto ", LazyModeAuto, 10},

		// auto:N spectrum
		{"auto:5", LazyModeAuto, 5},
		{"auto:10", LazyModeAuto, 10},
		{"auto:25", LazyModeAuto, 25},
		{"auto:99", LazyModeAuto, 99},

		// auto:0 / auto:100 → boundary aliases for true / false
		{"auto:0", LazyModeAlways, 0},
		{"auto:100", LazyModeStandard, 0},
		{"auto:-5", LazyModeAlways, 0},    // negative folds to "always"
		{"auto:200", LazyModeStandard, 0}, // over-100 folds to "never"

		// boolean truthy → always
		{"true", LazyModeAlways, 0},
		{"TRUE", LazyModeAlways, 0},
		{"1", LazyModeAlways, 0},
		{"yes", LazyModeAlways, 0},
		{"on", LazyModeAlways, 0},

		// boolean falsy → standard
		{"false", LazyModeStandard, 0},
		{"FALSE", LazyModeStandard, 0},
		{"0", LazyModeStandard, 0},
		{"no", LazyModeStandard, 0},
		{"off", LazyModeStandard, 0},

		// junk falls back to default
		{"garbage", LazyModeAuto, 10},
		{"auto:abc", LazyModeAuto, 10},
		{"maybe", LazyModeAuto, 10},
	}
	for _, c := range cases {
		t.Run("input="+c.in, func(t *testing.T) {
			gotMode, gotPct := parseEnableToolSearch(c.in)
			if gotMode != c.wantMode || gotPct != c.wantPct {
				t.Errorf("parseEnableToolSearch(%q) = (%d, %d), want (%d, %d)",
					c.in, gotMode, gotPct, c.wantMode, c.wantPct)
			}
		})
	}
}
