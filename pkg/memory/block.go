// Package memory exposes the public Block type — Metis's unit of
// core (in-context) memory.
//
// Plugin authors interact with memory mostly through hooks (e.g. a
// PostToolUse handler that updates a block when a session ends).
// Today we only export the Block struct; the in-process Manager and
// search index stay internal because their API is still evolving and
// 3rd parties wanting persistent recall typically just call into the
// agent's existing memory tool from their own tool plugin.
//
// Pairs with pkg/llm, pkg/tool, pkg/hook, pkg/channel, pkg/skill —
// the six-pillar plugin SDK as of this round.
package memory

// Block is one labeled chunk of core memory.
//
// Persisted as a labeled Markdown block under the canonical
// $METIS_HOME/memory/core.d directory, but plugin authors should treat it as
// read-mostly: write operations go through the Memory tool, not by
// constructing Blocks directly. Construction here is for
// deserialization (e.g. plugins that want to introspect what memory
// labels exist).
type Block struct {
	ID        string `json:"id"`
	Label     string `json:"label"` // e.g., "user", "system", "working"
	Content   string `json:"content"`
	MaxChars  int    `json:"max_chars"` // soft cap; manager truncates over-long writes
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
