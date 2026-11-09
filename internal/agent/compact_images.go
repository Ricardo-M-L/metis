package agent

// Legacy explicit newest-N helpers. The automatic compactor no longer uses
// this window mapping: compact_image_policy.go selects native visual evidence
// by actual context pressure, task relevance and visual/wire budgets. Keeping
// these helpers preserves the explicit API without rewriting cached history
// merely because a fourth screenshot arrived.

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// DefaultKeepRecentImageBlocks is retained for callers of the legacy explicit
// count-based helper. The automatic compactor uses the budget policy instead.
const DefaultKeepRecentImageBlocks = 3

// keepRecentImagesFor is the legacy explicit keep-N mapping. Native image
// cost is independent of base64 transport length; this mapping must not be
// used for automatic context-pressure or summary-admission decisions.
//
// Unknown windows keep the historical fallback for API compatibility.
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

// ImagePruneSentinelTemplate is the legacy count-based placeholder. Its byte
// count describes transport size, never the image's native visual token cost.
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

// PruneAllImages is a legacy explicit count-based operation. Automatic
// compaction uses fitImagesToContextBudget and retains every image that fits
// the native visual allowance, regardless of its base64 transport length.
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
