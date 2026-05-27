package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// mkImage builds a synthetic image content block with a
// recognisable Data field so test assertions can detect which
// images were kept vs replaced.
func mkImage(tag string) llm.ContentBlock {
	return llm.ContentBlock{
		Type:      "image",
		MediaType: "image/jpeg",
		Data:      "BASE64_" + tag,
	}
}

func mkTextImg(s string) llm.ContentBlock {
	return llm.ContentBlock{Type: "text", Text: s}
}

func mkToolResultWithImage(id, txt, imgTag string) llm.ContentBlock {
	return llm.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  id,
		ToolResult: txt,
		ToolResultBlocks: []llm.ContentBlock{
			mkTextImg(txt),
			mkImage(imgTag),
		},
	}
}

// TestPruneOldImages_KeepN_TopLevel — the canonical case: a
// straight sequence of top-level image blocks. Newest N survive,
// older ones turn into text placeholders.
func TestPruneOldImages_KeepN_TopLevel(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("oldest")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("middle")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("recent")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("newest")}},
	}
	out, pruned := PruneOldImages(msgs, 2)
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2 (4 images − keep 2)", pruned)
	}
	// Newest two preserved as image; older two replaced as text.
	if out[0].Content[0].Type != "text" {
		t.Errorf("oldest should be text placeholder; got Type=%q", out[0].Content[0].Type)
	}
	if !strings.Contains(out[0].Content[0].Text, "image cleared") {
		t.Errorf("placeholder missing sentinel; got %q", out[0].Content[0].Text)
	}
	if out[1].Content[0].Type != "text" {
		t.Errorf("middle should be text placeholder; got Type=%q", out[1].Content[0].Type)
	}
	if out[2].Content[0].Type != "image" || out[2].Content[0].Data != "BASE64_recent" {
		t.Errorf("recent should survive as image; got %+v", out[2].Content[0])
	}
	if out[3].Content[0].Type != "image" || out[3].Content[0].Data != "BASE64_newest" {
		t.Errorf("newest should survive as image; got %+v", out[3].Content[0])
	}
}

// TestPruneOldImages_DoesNotMutateInput — the prune must produce
// a fresh slice; mutating the caller's history would corrupt any
// concurrent reader (e.g. the streaming UI thread).
func TestPruneOldImages_DoesNotMutateInput(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("a")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("b")}},
	}
	_, _ = PruneOldImages(msgs, 1)
	if msgs[0].Content[0].Type != "image" || msgs[0].Content[0].Data != "BASE64_a" {
		t.Errorf("input was mutated; got %+v", msgs[0].Content[0])
	}
}

// TestPruneOldImages_NoopWhenAllFit — fewer images than keepN ⇒
// every image survives and pruned count is 0. Defensive: a single-
// screenshot session must NOT pay the placeholder swap.
func TestPruneOldImages_NoopWhenAllFit(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("only")}},
	}
	out, pruned := PruneOldImages(msgs, 3)
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0", pruned)
	}
	if out[0].Content[0].Type != "image" {
		t.Errorf("only image should survive; got %+v", out[0].Content[0])
	}
}

// TestPruneOldImages_KeepZeroDisabled — keepN<=0 is the off
// switch (advanced opt-out for caching-sensitive users); every
// image must survive untouched.
func TestPruneOldImages_KeepZeroDisabled(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("a"), mkImage("b"), mkImage("c")}},
	}
	out, pruned := PruneOldImages(msgs, 0)
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0 when keepN<=0", pruned)
	}
	for i, b := range out[0].Content {
		if b.Type != "image" {
			t.Errorf("block %d altered with keepN=0; got Type=%q", i, b.Type)
		}
	}
}

// TestPruneOldImages_Idempotent — running twice in a row must
// produce the same pruned slice and 0 prunes on the second pass.
// The compactor pipeline calls Snip / Microcompact / Compact
// sequentially and each can re-enter maybeCompact via the auto
// scheduler — double-prune must not compound.
func TestPruneOldImages_Idempotent(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("a")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("b")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("c")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("d")}},
	}
	once, prunedOnce := PruneOldImages(msgs, 2)
	if prunedOnce != 2 {
		t.Fatalf("first pass pruned = %d, want 2", prunedOnce)
	}
	twice, prunedTwice := PruneOldImages(once, 2)
	if prunedTwice != 0 {
		t.Errorf("second pass pruned = %d, want 0 (idempotent)", prunedTwice)
	}
	// Sanity: text-block placeholders aren't promoted back to images.
	for i, m := range twice {
		if m.Content[0].Type != once[i].Content[0].Type {
			t.Errorf("msg[%d] type changed on second pass: %q → %q",
				i, once[i].Content[0].Type, m.Content[0].Type)
		}
	}
}

// TestPruneOldImages_InsideToolResult — cu workflow shape: every
// state-changing tool_result has a {text, image} body. Pruning
// must reach INTO the nested ToolResultBlocks slice, leaving the
// outer tool_result framing (ToolUseID, ToolResult text) intact
// so model→tool dispatch tracking stays consistent.
func TestPruneOldImages_InsideToolResult(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkToolResultWithImage("tu1", "before", "old_screen")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkToolResultWithImage("tu2", "after", "new_screen")}},
	}
	out, pruned := PruneOldImages(msgs, 1)
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}
	// Outer tool_result must be untouched.
	if out[0].Content[0].Type != "tool_result" {
		t.Errorf("outer tool_result type changed; got %q", out[0].Content[0].Type)
	}
	if out[0].Content[0].ToolUseID != "tu1" {
		t.Errorf("ToolUseID dropped from prune; got %q", out[0].Content[0].ToolUseID)
	}
	if out[0].Content[0].ToolResult != "before" {
		t.Errorf("ToolResult text dropped from prune; got %q", out[0].Content[0].ToolResult)
	}
	// Inner image swapped for placeholder.
	subBlocks := out[0].Content[0].ToolResultBlocks
	if len(subBlocks) != 2 {
		t.Fatalf("sub-block count changed: got %d, want 2", len(subBlocks))
	}
	if subBlocks[0].Text != "before" {
		t.Errorf("inner text block lost; got %q", subBlocks[0].Text)
	}
	if subBlocks[1].Type != "text" || !strings.Contains(subBlocks[1].Text, "image cleared") {
		t.Errorf("inner image not replaced with placeholder; got %+v", subBlocks[1])
	}
	// Newest tool_result's image still alive.
	if out[1].Content[0].ToolResultBlocks[1].Type != "image" {
		t.Errorf("most recent inner image was pruned; got %+v", out[1].Content[0].ToolResultBlocks[1])
	}
}

// TestPruneOldImages_PlaceholderCarriesMetadata — the placeholder
// must mention the original MediaType + byte size so the model
// can reason about "there was a 130KB screenshot here". Lets the
// model decide whether to re-screenshot rather than failing
// silently.
func TestPruneOldImages_PlaceholderCarriesMetadata(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "image", MediaType: "image/jpeg", Data: strings.Repeat("X", 130000)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("recent")}},
	}
	out, _ := PruneOldImages(msgs, 1)
	ph := out[0].Content[0]
	if ph.Type != "text" {
		t.Fatalf("not pruned: %+v", ph)
	}
	if !strings.Contains(ph.Text, "image/jpeg") {
		t.Errorf("placeholder missing media type; got %q", ph.Text)
	}
	if !strings.Contains(ph.Text, "130000") {
		t.Errorf("placeholder missing byte budget; got %q", ph.Text)
	}
}

// TestPruneOldImages_PreservesUnrelatedBlocks — text, tool_use,
// thinking blocks must pass through untouched. Defensive: a
// regression that "prunes everything non-image" would silently
// lose conversation state.
func TestPruneOldImages_PreservesUnrelatedBlocks(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "thinking", Text: "planning the next step"},
			{Type: "text", Text: "I will take a screenshot"},
			{Type: "tool_use", ToolUseID: "tu1", ToolName: "screenshot"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkImage("only")}},
	}
	out, pruned := PruneOldImages(msgs, 1)
	if pruned != 0 {
		t.Errorf("nothing should prune (1 image, keep 1); got %d", pruned)
	}
	if out[0].Content[0].Type != "thinking" {
		t.Errorf("thinking block altered; got %+v", out[0].Content[0])
	}
	if out[0].Content[1].Type != "text" || out[0].Content[1].Text != "I will take a screenshot" {
		t.Errorf("text block altered; got %+v", out[0].Content[1])
	}
	if out[0].Content[2].Type != "tool_use" || out[0].Content[2].ToolUseID != "tu1" {
		t.Errorf("tool_use block altered; got %+v", out[0].Content[2])
	}
}

// TestKeepRecentImagesFor — pin the maxCtx → keepN mapping. Adding
// a new tier (e.g. claude-haiku 100k or gemini-flash 8M) means
// adding a row here too; pin guards against silent off-by-one
// regressions when the constants shift.
func TestKeepRecentImagesFor(t *testing.T) {
	cases := []struct {
		name    string
		maxCtx  int
		want    int
	}{
		{"unknown defaults to 3", 0, DefaultKeepRecentImageBlocks},
		{"unknown-negative same", -1, DefaultKeepRecentImageBlocks},
		{"kimi 262k → 1", 262_144, 1},
		{"gpt-4o 128k → 1", 128_000, 1},
		{"deepseek 256k → 1", 256_000, 1},
		{"300k boundary → 2", 300_000, 2},
		{"500k → 2", 500_000, 2},
		{"600k boundary → 3", 600_000, 3},
		{"claude 200k → 1 (small ctx)", 200_000, 1},
		{"minimax 1M → 3", 1_000_000, 3},
		{"gemini 2M → 3", 2_000_000, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepRecentImagesFor(tc.maxCtx); got != tc.want {
				t.Errorf("keepRecentImagesFor(%d) = %d, want %d", tc.maxCtx, got, tc.want)
			}
		})
	}
}

// TestEstimateContentBlockTokens_ImageData — regression for the
// estimator bug surfaced 2026-05-27: image Data was completely
// invisible to the estimator (only Text / ToolResult counted),
// so Kimi sessions blew the 262k cap while the local estimator
// said "you're at 80k, all good." After the fix the estimator
// should attribute ~0.75 token/char for base64 image payloads.
func TestEstimateContentBlockTokens_ImageData(t *testing.T) {
	const base64Len = 130_000 // realistic 1280x800 JPEG q=85
	imgBlock := llm.ContentBlock{
		Type:      "image",
		MediaType: "image/jpeg",
		Data:      strings.Repeat("X", base64Len),
	}
	got := estimateContentBlockTokens(imgBlock)
	wantLo := int(float64(base64Len) * 0.7)
	wantHi := int(float64(base64Len) * 0.8)
	if got < wantLo || got > wantHi {
		t.Errorf("estimate(130KB image) = %d tokens, want ~%d–%d (≈0.75 tok/char)", got, wantLo, wantHi)
	}
	// Defensive: a non-image block of the same shape must NOT
	// trigger the image branch.
	text := llm.ContentBlock{Type: "text", Text: strings.Repeat("X", base64Len)}
	textTok := estimateContentBlockTokens(text)
	if textTok > got/2 {
		t.Errorf("text of same length should cost MUCH less; text=%d image=%d", textTok, got)
	}
}

// TestEstimateContentBlockTokens_NestedImageInToolResult — the
// other half of the estimator bug: cu's vision-aware tool_result
// wraps screenshots inside ToolResultBlocks. Without the recursive
// walk the outer tool_result reports "8 tokens" no matter what's
// inside.
func TestEstimateContentBlockTokens_NestedImageInToolResult(t *testing.T) {
	const base64Len = 100_000
	tr := llm.ContentBlock{
		Type:      "tool_result",
		ToolUseID: "tu1",
		ToolResultBlocks: []llm.ContentBlock{
			{Type: "text", Text: "captured 1280x800 JPEG q=85"},
			{Type: "image", MediaType: "image/jpeg",
				Data: strings.Repeat("Y", base64Len)},
		},
	}
	got := estimateContentBlockTokens(tr)
	// At minimum it must reflect the image's ~75% multiplier.
	wantMin := int(float64(base64Len) * 0.7)
	if got < wantMin {
		t.Errorf("nested image not counted: tool_result with 100KB image estimate = %d, want >= %d", got, wantMin)
	}
}
