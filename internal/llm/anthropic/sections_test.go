package anthropic

// sections_test.go — locks the typed-section path through Convert.
// Before this wiring, every request flattened sections to a string and
// re-parsed for cache boundaries — losing the Volatile flag and
// causing memory updates to invalidate the addendum cache. The new
// path routes []SystemSection straight through chooseSystemBlocks →
// BuildSystemBlocksFromSections, preserving per-section cache intent.

import (
	"testing"

	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestChooseSystemBlocks_PrefersTypedSections(t *testing.T) {
	req := Request{
		System: "should-be-ignored", // string also set; sections must win
		SystemSections: []pubprov.SystemSection{
			{Name: "base", Body: "base prompt", Cache: true},
			{Name: "env", Body: "<env>cwd=/foo</env>", Cache: false, Volatile: true},
			{Name: "addendum", Body: "user addendum", Cache: true},
			{Name: "memory", Body: "memory body", Cache: false, Volatile: true},
		},
	}
	got := chooseSystemBlocks(req)
	if len(got) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(got))
	}
	if got[0].Text != "base prompt" || got[0].CacheControl == nil {
		t.Errorf("base block missing or uncached: %+v", got[0])
	}
	if got[1].CacheControl != nil {
		t.Errorf("env block should NOT be cached (Volatile=true)")
	}
	if got[2].CacheControl == nil {
		t.Errorf("addendum block should be cached")
	}
	if got[3].CacheControl != nil {
		t.Errorf("memory block (Volatile=true) must not be cached — that's the whole point")
	}
}

func TestChooseSystemBlocks_FallsBackToStringWhenNoSections(t *testing.T) {
	req := Request{
		System: "legacy " + SystemPromptCacheBoundary + " dynamic",
	}
	got := chooseSystemBlocks(req)
	if len(got) != 2 {
		t.Fatalf("expected 2 blocks from boundary split, got %d", len(got))
	}
	if got[0].CacheControl == nil {
		t.Errorf("legacy path: static prefix should still be cached")
	}
	if got[1].CacheControl != nil {
		t.Errorf("legacy path: dynamic suffix must not be cached")
	}
}

func TestBuildSystemBlocksFromSections_BudgetCap(t *testing.T) {
	// More cacheable sections than the per-system budget (2). The
	// first 2 get cache markers; the rest pass through as
	// plain text blocks (still emitted, just not cached).
	secs := []SystemSection{
		{Name: "a", Body: "a", Cache: true},
		{Name: "b", Body: "b", Cache: true},
		{Name: "c", Body: "c", Cache: true},
		{Name: "d", Body: "d", Cache: true},
	}
	out := BuildSystemBlocksFromSections(secs)
	if len(out) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(out))
	}
	cached := 0
	for _, blk := range out {
		if blk.CacheControl != nil {
			cached++
		}
	}
	if cached != 2 {
		t.Errorf("system-side cache cap not enforced: got %d cached blocks, want 2", cached)
	}
}

func TestBuildSystemBlocksFromSections_DropsEmptyBodies(t *testing.T) {
	secs := []SystemSection{
		{Name: "a", Body: "", Cache: true},
		{Name: "b", Body: "real", Cache: false},
		{Name: "c", Body: "", Cache: true},
	}
	out := BuildSystemBlocksFromSections(secs)
	if len(out) != 1 {
		t.Fatalf("expected only the non-empty section to make it through, got %d", len(out))
	}
	if out[0].Text != "real" {
		t.Errorf("wrong block survived: %q", out[0].Text)
	}
}
