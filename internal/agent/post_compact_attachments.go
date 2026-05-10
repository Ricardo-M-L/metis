package agent

// post_compact_attachments.go — re-inject lightweight file/skill
// pointers after Compact summarizes the early conversation.
//
// Why this exists: claude-code's POST_COMPACT_TOKEN_BUDGET pipeline
// re-attaches the actual file CONTENTS of recently-Read files (5K
// per file, 25K total) so the model can keep working without paying
// the round-trip of re-issuing Read calls. metis takes the lighter
// path: after Compact, we append a synthetic user message that just
// LISTS the file paths the agent recently touched (Read / Edit /
// Write). The model still has to call Read to get fresh content,
// but it doesn't lose track of WHAT was relevant — without this,
// the summary often loses concrete file paths and the model wastes
// a turn or two probing around to find what it was working on.
//
// This is intentionally smaller than claude-code's
// createPostCompactFileAttachments — content re-injection costs
// real tokens (5K × 5 = 25K guaranteed inflation), and we already
// have EnforcePostCompactBudget capping the request envelope. The
// path-pointer approach gives 90% of the continuity at <1% of the
// token cost.
//
// Skill listing post-compact (claude-code's stripReinjectedAttachments
// counterpart) is deferred — metis doesn't have skill discovery
// telemetry to feed it yet, and the user-visible cost is small.

import (
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// MaxPostCompactRecentFiles is the cap on file paths surfaced in the
// post-compact attachment block. Mirrors claude-code's recent-read
// window — 5 is enough for "what was I working on" context without
// drowning the synthetic message in a wall of paths.
const MaxPostCompactRecentFiles = 5

// extractRecentToolInputPaths walks messages newest-first and returns
// up to maxN distinct file paths from tool_use blocks for the listed
// tool names. De-duplicated by path; ordering preserves last-touched-
// first so the model sees the most relevant entries at the top.
//
// Tool names matched: "Read", "Edit", "Write". Glob/Grep are skipped
// (their inputs are patterns, not concrete files). NotebookEdit is
// out (rare; same input shape as Edit but different field name).
//
// Path extraction tries the common field names ("path", "file_path")
// in that order. A tool_use without a recognized path field is
// silently ignored — better to under-attach than to inject "path: -"
// stubs that confuse the model.
func extractRecentToolInputPaths(messages []llm.Message, maxN int) []string {
	if maxN <= 0 {
		return nil
	}
	var paths []string
	seen := make(map[string]bool)
	for i := len(messages) - 1; i >= 0; i-- {
		for _, b := range messages[i].Content {
			if b.Type != "tool_use" {
				continue
			}
			switch b.ToolName {
			case "Read", "Edit", "Write":
			default:
				continue
			}
			path := stringFieldAny(b.ToolInput, "path", "file_path")
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
			if len(paths) >= maxN {
				return paths
			}
		}
	}
	return paths
}

// stringFieldAny returns the first string-typed value from m at any of
// the listed keys. Returns "" when nothing matches — the absence is
// meaningful (caller skips the entry rather than emitting "path: ").
func stringFieldAny(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// BuildPostCompactAttachment renders the synthetic user message that
// gets appended to compacted history. Returns the empty Message zero
// value when there's nothing worth surfacing — callers should check
// `len(msg.Content) == 0` before appending.
//
// Format: a single text block listing the recent file paths in a
// `<post_compact_context>...</post_compact_context>` envelope so the
// model can recognize the synthetic provenance and weight it
// appropriately (it's a hint, not user intent).
func BuildPostCompactAttachment(messages []llm.Message) llm.Message {
	paths := extractRecentToolInputPaths(messages, MaxPostCompactRecentFiles)
	if len(paths) == 0 {
		return llm.Message{}
	}
	var b strings.Builder
	b.WriteString("<post_compact_context>\n")
	b.WriteString("Files the agent worked with before compaction (most-recent first):\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	b.WriteString("</post_compact_context>")
	return llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: b.String(),
		}},
	}
}
