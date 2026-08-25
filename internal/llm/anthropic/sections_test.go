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

func TestBuildSystemBlocksFromSections_MarksEndOfStableRun(t *testing.T) {
	// A cache breakpoint covers the complete prefix before it. Four
	// adjacent stable sections therefore need one marker on the last
	// section, not two markers on the first two short fragments.
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
	for i := 0; i < len(out)-1; i++ {
		if out[i].CacheControl != nil {
			t.Fatalf("stable prefix marker placed too early at block %d", i)
		}
	}
	if out[len(out)-1].CacheControl == nil {
		t.Fatal("last block of the stable run must carry the cache breakpoint")
	}
}

func TestBuildSystemBlocksFromSections_RuntimeSnapshotExtendsStablePrefix(t *testing.T) {
	secs := []SystemSection{
		{Name: "base", Body: "base", Cache: true},
		{Name: "runtime_state", Body: "permission_mode: default", Cache: true},
		{Name: "memory", Body: "changing memory", Volatile: true},
	}
	out := BuildSystemBlocksFromSections(secs)
	if out[0].CacheControl != nil {
		t.Fatal("base marker should be delayed through the stable runtime snapshot")
	}
	if out[1].CacheControl == nil {
		t.Fatal("runtime snapshot must close the reusable stable prefix")
	}
	if out[2].CacheControl != nil {
		t.Fatal("volatile memory must stay outside the runtime-state breakpoint")
	}
}

func TestBuildSystemBlocksFromSections_UsesTwoStableRunBoundaries(t *testing.T) {
	secs := []SystemSection{
		{Name: "identity", Body: "identity", Cache: true},
		{Name: "rules", Body: "rules", Cache: true},
		{Name: "project", Body: "project", Cache: false},
		{Name: "addendum", Body: "addendum", Cache: true},
		{Name: "env", Body: "env", Volatile: true},
	}
	out := BuildSystemBlocksFromSections(secs)
	if out[0].CacheControl != nil || out[1].CacheControl == nil {
		t.Fatalf("first marker must protect the whole base prefix: %+v", out)
	}
	if out[2].CacheControl != nil || out[3].CacheControl == nil || out[4].CacheControl != nil {
		t.Fatalf("second marker must end at the stable addendum before env: %+v", out)
	}
}

func TestToAnthropic_CacheBreakpointBudgetUsesNewestMessageOnly(t *testing.T) {
	req := Request{
		SystemSections: []pubprov.SystemSection{
			{Name: "base", Body: "base prompt", Cache: true},
			{Name: "project", Body: "project context", Cache: false},
			{Name: "addendum", Body: "stable addendum", Cache: true},
		},
		Tools: []ToolSpec{{Name: "Read", Description: "read a file", InputSchema: map[string]any{"type": "object"}}},
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "first request"}}},
			{Role: RoleAssistant, Content: []ContentBlock{{Type: "text", Text: "first response"}}},
			{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "follow-up"}}},
		},
	}

	got := toAnthropic(req, "claude-sonnet-4", 1024)
	markers := 0
	for _, block := range got.System {
		if block.CacheControl != nil {
			markers++
		}
	}
	for _, tool := range got.Tools {
		if tool.CacheControl != nil {
			markers++
		}
	}
	for _, message := range got.Messages {
		for _, block := range message.Content {
			if block.CacheControl != nil {
				markers++
			}
		}
	}
	if markers != 4 {
		t.Fatalf("Anthropic accepts at most 4 cache breakpoints; got %d", markers)
	}
	if got.Messages[0].Content[0].CacheControl != nil {
		t.Fatal("older user message must not consume a cache breakpoint")
	}
	if got.Messages[2].Content[0].CacheControl == nil {
		t.Fatal("newest user message must carry the rolling cache breakpoint")
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
