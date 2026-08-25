package agent

// compact_images.go — keep-N-most-recent-images pass for the
// compactor pipeline. Mirrors:
//
//   * Anthropic computer-use-demo's `_maybe_filter_to_n_most_recent_images`
//     (computer-use-demo/computer_use_demo/loop.py:193) — the official
//     reference says image attachments in older tool results should be
//     dropped once they fall outside the most-recent window.
//   * claude-code's services/compact/microCompact `keepRecent` (default
//     5) which replaces older tool_result content with the literal text
//     "[Old tool result content cleared]".
//
// Why we need this in metis. cu workloads attach a fresh screenshot
// to every state-changing tool_result by default (after_action.go,
// matching Anthropic's reference). A typical Kimi session at 262k
// ctx + 5-6 screenshots ×130KB JPEG ≈ 200k tokens of image alone
// blows the model's input limit before the agent can finish the
// task. Session 87e366f post-mortem (2026-05-27, Safari search via
// Kimi): hit `Invalid request: Your request exceeded model token
// limit: 262144 (requested: 351175)` on step 6.
//
// Design constraints
//
//   * Don't touch the most recent N image blocks — those are what
//     the model needs to act on right now.
//   * Don't touch tool_use / tool_result framing — only the image
//     content INSIDE a tool_result block. The model's plan-tracking
//     depends on seeing which tool emitted which result; throwing
//     away the tool_use_id pairing would confuse it more than
//     keeping the image would.
//   * Preserve a textual marker so the model knows "there was an
//     image here, it's been cleared because it's old" rather than
//     silently disappearing the data. Mirrors claude-code's
//     "[Old tool result content cleared]" sentinel.
//   * Idempotent: running PruneOldImages twice in a row on the
//     same history is a no-op. The compactor pipeline calls Snip /
//     Microcompact / Compact sequentially; image pruning can be
//     interleaved without compounding the prune.
//
// Out of scope for this pass (deliberately):
//
//   * Cache invalidation. Replacing image bytes deep in history
//     does invalidate any prompt-cache breakpoint that lived after
//     the replacement; the 5-minute Anthropic cache TTL ages those
//     out anyway. The Anthropic demo opts to disable image pruning
//     entirely when caching is on; we keep it on because (a) cu
//     screenshots eat context faster than caching saves it, and
//     (b) metis users who care about caching can set
//     KeepRecentImageBlocks = 0 to opt out.

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// DefaultKeepRecentImageBlocks is the fallback fan-out when we can't
// read the active model's context window (early boot, tests). Sits
// between claude-code's 5 (IDE sessions, 200k+ models) and Anthropic
// computer-use-demo's typical 3 (cu workloads).
const DefaultKeepRecentImageBlocks = 3

// keepRecentImagesFor returns how many image blocks to retain given
// the model's max context window. Image base64 is near-1:1 against
// the tokeniser, so each 130KB JPEG screenshot ≈ 90-100k tokens of
// real server-side budget. Smaller windows have to prune more
// aggressively or every multi-step GUI task instantly hits the cap.
//
// Mapping (empirical from session 87e366f / Kimi):
//
//	maxCtx < 300k  → keep 1   (Kimi-K2.6 = 262k, GPT-4o = 128k)
//	300k–600k      → keep 2
//	>= 600k        → keep 3   (Claude 200k+, MiniMax 1M, Gemini 2M)
//
// 0 is a valid override that disables pruning entirely (cache-
// sensitive users on huge windows can pass it). maxCtx <= 0 means
// "I don't know" and falls back to DefaultKeepRecentImageBlocks.
func keepRecentImagesFor(maxCtx int) int {
	if maxCtx <= 0 {
		return DefaultKeepRecentImageBlocks
	}
	switch {
	case maxCtx < 300_000:
		return 1
	case maxCtx < 600_000:
		return 2
	default:
		return 3
	}
}

// ImagePruneSentinel is the placeholder text the model sees in
// place of a pruned image. Names BOTH the original media type and
// the rough byte budget so the model can reason about "ok, there
// was a 130KB screenshot here" without us re-shipping the bytes.
// Format chosen for easy grep-ability in transcripts.
const ImagePruneSentinelTemplate = "[image cleared by compactor — %s, was %d bytes base64; %d most recent screenshots kept in context]"

// PruneOldImages walks `messages` from newest to oldest, counts
// image content blocks, and replaces every image past the first
// `keepN` with a text placeholder. Modifies a deep copy; the
// original `messages` slice is untouched (callers that need to
// re-prune from a known baseline rely on this).
//
// Returns (newMessages, prunedCount). prunedCount is exported via
// the compactor's existing "I shortened things" event path so the
// user sees one info line per prune ("image-pruned 6 older
// screenshots") instead of silent shrinkage.
//
// keepN <= 0 → no-op (pruning disabled).
//
// Idempotency: running on already-pruned history produces 0
// prunes because text-block placeholders aren't counted as images.
func PruneOldImages(messages []llm.Message, keepN int) ([]llm.Message, int) {
	if keepN <= 0 {
		return messages, 0
	}
	return pruneImages(messages, keepN)
}

// PruneAllImages is the full-checkpoint variant. Automatic lightweight image
// pruning keeps a few recent screenshots for active visual work, but a paid
// full compaction must be able to meet its post-compact budget even when one
// recent base64 image would otherwise consume hundreds of thousands of tokens.
func PruneAllImages(messages []llm.Message) ([]llm.Message, int) {
	return pruneImages(messages, 0)
}

func pruneImages(messages []llm.Message, keepN int) ([]llm.Message, int) {
	// Deep-enough copy: llm.Message.Content is a slice we
	// mutate, so we copy each Content slice. ContentBlock itself
	// is a value type containing only strings / maps / nested
	// slices, but the slices we'd modify (ToolResultBlocks) need
	// their own copy too — see pruneInsideToolResult below.
	out := make([]llm.Message, len(messages))
	for i, m := range messages {
		nc := make([]llm.ContentBlock, len(m.Content))
		copy(nc, m.Content)
		out[i] = llm.Message{Role: m.Role, Content: nc}
	}

	kept := 0
	pruned := 0
	// Walk newest → oldest. For each image we encounter (whether
	// top-level or nested inside a tool_result.ToolResultBlocks),
	// the first `keepN` survive, every subsequent one becomes a
	// text block.
	for i := len(out) - 1; i >= 0; i-- {
		for j := len(out[i].Content) - 1; j >= 0; j-- {
			b := out[i].Content[j]
			switch b.Type {
			case "image":
				if kept < keepN {
					kept++
					continue
				}
				out[i].Content[j] = makePlaceholder(b.MediaType, len(b.Data), keepN)
				pruned++
			case "tool_result":
				// Multi-part tool_result body. Walk its nested
				// blocks the same way; rewrites land back on the
				// outer block via a fresh slice so we don't
				// mutate the caller's data.
				if len(b.ToolResultBlocks) == 0 {
					continue
				}
				newKept, newPruned, newSubBlocks := pruneInsideToolResult(b.ToolResultBlocks, keepN, kept)
				kept = newKept
				pruned += newPruned
				if newPruned > 0 {
					out[i].Content[j].ToolResultBlocks = newSubBlocks
				}
			}
		}
	}
	return out, pruned
}

// pruneInsideToolResult is the recursive (per-tool_result) prune
// applied to the multi-part body of a ToolResultBlocks slice.
// Returns updated kept counter, prunes performed, and a rewritten
// sub-block slice (the slice is only reallocated when at least one
// block changed — saves an alloc per untouched tool_result).
func pruneInsideToolResult(blocks []llm.ContentBlock, keepN, kept int) (int, int, []llm.ContentBlock) {
	pruned := 0
	var rewrite []llm.ContentBlock
	for j := len(blocks) - 1; j >= 0; j-- {
		b := blocks[j]
		if b.Type != "image" {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		if rewrite == nil {
			rewrite = make([]llm.ContentBlock, len(blocks))
			copy(rewrite, blocks)
		}
		rewrite[j] = makePlaceholder(b.MediaType, len(b.Data), keepN)
		pruned++
	}
	if rewrite == nil {
		return kept, pruned, blocks
	}
	return kept, pruned, rewrite
}

// makePlaceholder builds the text-content-block that replaces a
// pruned image. Always Type="text" — preserves the block count so
// any provider that indexes content by position (none today, but
// defensive) doesn't shift.
func makePlaceholder(mediaType string, base64Bytes int, keepN int) llm.ContentBlock {
	if mediaType == "" {
		mediaType = "image/*"
	}
	return llm.ContentBlock{
		Type: "text",
		Text: fmt.Sprintf(ImagePruneSentinelTemplate, mediaType, base64Bytes, keepN),
	}
}
