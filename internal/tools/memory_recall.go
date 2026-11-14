package tools

// MemoryRecallMetaKey identifies host-generated provenance for an explicit
// Memory.search/read result. It is internal result metadata, never parsed from
// memory text or exposed as model-controlled input.
const MemoryRecallMetaKey = "metis.memory.recall.v1"

// MemoryRecallReference uses the same serialized ID/Hash fields as the agent's
// recall fences. Hash covers the original content, scope, and source session.
type MemoryRecallReference struct{ ID, Hash string }

// MemoryRecallPassage retains only the source data the agent needs to verify
// an admitted result and record usage. Tags distinguish topic and archive IDs.
type MemoryRecallPassage struct {
	ID              string   `json:"id"`
	Content         string   `json:"content"`
	CreatedAt       string   `json:"created_at,omitempty"`
	Scope           string   `json:"scope,omitempty"`
	SourceSessionID string   `json:"source_session_id,omitempty"`
	Tags            []string `json:"tags,omitempty"`
}

// MemoryRecallResult travels only between the built-in tool and the agent's
// dispatcher. Producing it does not record consumption: results can still be
// interrupted, replaced by budget stubs, or withdrawn before a model request.
type MemoryRecallResult struct {
	References        []MemoryRecallReference `json:"references"`
	Passages          []MemoryRecallPassage   `json:"passages"`
	ContextLineHashes []string                `json:"context_lines,omitempty"`
}
