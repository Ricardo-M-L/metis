package router

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
)

// fixture: a tiny catalog with enough variety to exercise every Hint
// field. Three providers, six models, distinct cost / context /
// capability matrices.
func fixture() catalog.Catalog {
	return catalog.Catalog{
		"anthropic": catalog.Provider{
			ID: "anthropic",
			Models: map[string]catalog.Model{
				"claude-haiku-4-5": {
					ID: "claude-haiku-4-5", Attachment: true, Reasoning: true, ToolCall: true,
					Cost:  catalog.Cost{Input: 1.0, Output: 5.0},
					Limit: catalog.Limit{Context: 200_000},
				},
				"claude-sonnet-4-6": {
					ID: "claude-sonnet-4-6", Attachment: true, Reasoning: true, ToolCall: true,
					Cost:  catalog.Cost{Input: 3.0, Output: 15.0},
					Limit: catalog.Limit{Context: 200_000},
				},
			},
		},
		"openai": catalog.Provider{
			ID: "openai",
			Models: map[string]catalog.Model{
				"gpt-mini": {
					ID: "gpt-mini", Attachment: false, Reasoning: false, ToolCall: true,
					Cost:  catalog.Cost{Input: 0.15, Output: 0.60},
					Limit: catalog.Limit{Context: 128_000},
				},
				"gpt-vision-pro": {
					ID: "gpt-vision-pro", Attachment: true, Reasoning: false, ToolCall: true,
					Cost:  catalog.Cost{Input: 2.5, Output: 10.0},
					Limit: catalog.Limit{Context: 128_000},
				},
			},
		},
		"deepseek": catalog.Provider{
			ID: "deepseek",
			Models: map[string]catalog.Model{
				"r1-deprecated": {
					ID: "r1-deprecated", ToolCall: true, Reasoning: true,
					Cost:   catalog.Cost{Input: 0.05, Output: 0.20},
					Limit:  catalog.Limit{Context: 64_000},
					Status: "deprecated",
				},
			},
		},
	}
}

func TestPick_AttachmentRequired(t *testing.T) {
	got := Pick(fixture(), Hint{NeedAttachment: true})
	// gpt-mini lacks attachment, so the cheapest attachment-capable
	// candidate (claude-haiku-4-5 at $1.0) should win over gpt-vision-pro ($2.5)
	// and claude-sonnet-4-6 ($3.0).
	if got.Provider != "anthropic" || got.Model != "claude-haiku-4-5" {
		t.Errorf("attachment+cheap should pick anthropic/claude-haiku-4-5; got %+v", got)
	}
}

func TestPick_NoConstraintsCheapestWins(t *testing.T) {
	got := Pick(fixture(), Hint{})
	// r1-deprecated is cheapest ($0.05) but deprecated-by-default is excluded.
	// gpt-mini at $0.15 should win.
	if got.Provider != "openai" || got.Model != "gpt-mini" {
		t.Errorf("no-constraints should pick cheapest non-deprecated (gpt-mini); got %+v", got)
	}
}

func TestPick_AllowDeprecatedUnlocksCheaper(t *testing.T) {
	got := Pick(fixture(), Hint{AllowDeprecated: true})
	if got.Provider != "deepseek" || got.Model != "r1-deprecated" {
		t.Errorf("AllowDeprecated should pick the $0.05 deprecated model; got %+v", got)
	}
}

func TestPick_ReasoningRequired(t *testing.T) {
	got := Pick(fixture(), Hint{NeedReasoning: true})
	// Reasoning models: claude-haiku-4-5 ($1.0), claude-sonnet-4-6 ($3.0).
	// (r1-deprecated is excluded by default.)
	if got.Provider != "anthropic" || got.Model != "claude-haiku-4-5" {
		t.Errorf("reasoning should pick claude-haiku-4-5; got %+v", got)
	}
}

func TestPick_MinContextFilters(t *testing.T) {
	// Need 150K context — only the 200K Anthropic models qualify.
	got := Pick(fixture(), Hint{MinContext: 150_000})
	if got.Provider != "anthropic" {
		t.Errorf("MinContext=150K should restrict to anthropic; got %+v", got)
	}
}

func TestPick_MaxCostFilters(t *testing.T) {
	// Cost ceiling $0.5/M — only gpt-mini qualifies.
	got := Pick(fixture(), Hint{MaxInputCost: 0.5})
	if got.Provider != "openai" || got.Model != "gpt-mini" {
		t.Errorf("MaxInputCost=0.5 should pick gpt-mini; got %+v", got)
	}
}

func TestPick_PreferProviderTiebreak(t *testing.T) {
	// Cap input cost so haiku ($1.0) and gpt-vision-pro ($2.5) stay in the pool.
	// Without preference, haiku wins on raw cost. With prefer=openai,
	// vision-pro's effective cost becomes $2.25 — still higher than haiku's
	// $1.0, so haiku wins. To exercise the discount, push cap so haiku is
	// excluded on cost while vision-pro discount makes it pass.
	got := Pick(fixture(), Hint{
		NeedAttachment: true,
		PreferProvider: "openai",
	})
	// haiku $1.0 < vision-pro $2.5*0.9=$2.25 → haiku still wins.
	if got.Provider != "anthropic" {
		t.Errorf("non-tied case: anthropic still cheapest; got %+v", got)
	}

	// Synthetic tied case: two models at exactly $2.0 with prefer.
	cat := catalog.Catalog{
		"a": catalog.Provider{ID: "a", Models: map[string]catalog.Model{
			"x": {ID: "x", ToolCall: true, Cost: catalog.Cost{Input: 2.0}, Limit: catalog.Limit{Context: 100_000}},
		}},
		"b": catalog.Provider{ID: "b", Models: map[string]catalog.Model{
			"y": {ID: "y", ToolCall: true, Cost: catalog.Cost{Input: 2.0}, Limit: catalog.Limit{Context: 100_000}},
		}},
	}
	got = Pick(cat, Hint{NeedToolCall: true, PreferProvider: "b"})
	if got.Provider != "b" {
		t.Errorf("preferred provider should win on tie; got %+v", got)
	}
}

func TestPick_NoMatchReturnsZero(t *testing.T) {
	// Ask for impossible: 1M context.
	got := Pick(fixture(), Hint{MinContext: 1_000_000})
	if got.Provider != "" || got.Model != "" {
		t.Errorf("no match should return zero Choice; got %+v", got)
	}
}

func TestMatch_HandlesZeroCatalogValues(t *testing.T) {
	// Models with missing pricing/context shouldn't be auto-rejected
	// when the user asks for cheap or long-context.
	m := catalog.Model{ToolCall: true} // zero Cost, zero Limit
	if !(Hint{NeedToolCall: true, MinContext: 100_000, MaxInputCost: 5.0}).Match(m) {
		t.Error("zero-value cost/context fields should be treated as 'unknown' (pass), not 'fail'")
	}
}

func TestPickAll_OrderedAndComplete(t *testing.T) {
	got := PickAll(fixture(), Hint{NeedAttachment: true})
	// Three attachment-capable: haiku ($1), vision-pro ($2.5), sonnet ($3).
	if len(got) != 3 {
		t.Fatalf("PickAll should return 3 attachment models; got %d (%+v)", len(got), got)
	}
	if got[0].Model != "claude-haiku-4-5" {
		t.Errorf("position 0 should be cheapest (haiku); got %s", got[0].Model)
	}
	if got[2].Model != "claude-sonnet-4-6" {
		t.Errorf("position 2 should be priciest (sonnet); got %s", got[2].Model)
	}
}

func TestReasonsFor_AllFields(t *testing.T) {
	r := reasonsFor(Hint{
		NeedAttachment: true, NeedReasoning: true, NeedToolCall: true,
		MinContext: 1, MaxInputCost: 1.0, PreferProvider: "x", AllowDeprecated: true,
	})
	if len(r) != 7 {
		t.Errorf("expected 7 reasons; got %d (%v)", len(r), r)
	}
}

func TestReasonsFor_EmptyHint(t *testing.T) {
	if r := reasonsFor(Hint{}); len(r) != 0 {
		t.Errorf("empty hint should have no reasons; got %v", r)
	}
}
