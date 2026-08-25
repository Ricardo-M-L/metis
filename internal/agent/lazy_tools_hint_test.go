package agent

// lazy_tools_hint_test.go — locks the searchHint weight added 2026-06-11
// (claude-code parity: Tool.searchHint, Tool.ts:371). A curated hint
// must (a) satisfy the required-term filter and (b) outrank a
// description-only match, without disturbing name-match dominance.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// hintedMCPTool wraps fakeMCPTool with a curated SearchHint.
type hintedMCPTool struct {
	fakeMCPTool
	hint string
}

func (h hintedMCPTool) SearchHint() string { return h.hint }

func TestSearchHintOutranksDescriptionMatch(t *testing.T) {
	reg := tools.NewRegistry()
	// Both tools mention "screenshot" only outside their names; the
	// hinted one carries it in the curated hint (+4) vs description (+2).
	reg.Register(hintedMCPTool{
		fakeMCPTool: mcpFake("mcp__alpha__grab", "captures the visible page"),
		hint:        "take browser screenshot",
	})
	reg.Register(mcpFake("mcp__beta__capture", "saves a screenshot of the page"))
	l := &Loop{Registry: reg}

	matches := searchToolsWithKeywords(l, "screenshot", 5)
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(matches))
	}
	if matches[0]["name"] != "mcp__alpha__grab" {
		t.Errorf("hinted tool should rank first; got %v", matches[0]["name"])
	}
}

// A required (+prefixed) term satisfied ONLY by the hint must not
// disqualify the tool.
func TestRequiredTermSatisfiedByHint(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(hintedMCPTool{
		fakeMCPTool: mcpFake("mcp__alpha__grab", "captures the visible page"),
		hint:        "take browser screenshot",
	})
	l := &Loop{Registry: reg}

	matches := searchToolsWithKeywords(l, "+screenshot grab", 5)
	if len(matches) != 1 {
		t.Fatalf("hint-only required match filtered out; matches = %d, want 1", len(matches))
	}
}

// Exact name-part match (+12) still dominates a hint match (+4).
func TestNameMatchStillBeatsHint(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(mcpFake("mcp__shot__screenshot", "grabs pixels"))
	reg.Register(hintedMCPTool{
		fakeMCPTool: mcpFake("mcp__alpha__grab", "captures the page"),
		hint:        "screenshot helper",
	})
	l := &Loop{Registry: reg}

	matches := searchToolsWithKeywords(l, "screenshot", 5)
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(matches))
	}
	if matches[0]["name"] != "mcp__shot__screenshot" {
		t.Errorf("name match should rank first; got %v", matches[0]["name"])
	}
}

func TestToolSearchKeywordIncludesBuiltInTools(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(mcpFake("Agent", "spawn parallel subagents for independent tasks"))
	reg.Register(mcpFake("mcp__gortex__explore", "explore indexed source code"))
	l := &Loop{Registry: reg}

	matches := searchToolsWithKeywords(l, "agent task spawn parallel subagent", 5)
	if len(matches) == 0 {
		t.Fatal("built-in Agent was omitted from ToolSearch keyword results")
	}
	if matches[0]["name"] != "Agent" {
		t.Fatalf("top match = %v, want Agent", matches[0]["name"])
	}
}
