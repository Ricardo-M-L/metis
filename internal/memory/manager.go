// Package memory implements Metis's multi-tier memory system.
// Inspired by MemGPT's block-based memory and Hermes's pluggable providers.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory/security"
	pubmem "github.com/Ricardo-M-L/metis/pkg/memory"
)

// ============================================================================
// Block - Core Memory Unit
// ============================================================================

// Block is an alias for the public type in pkg/memory. In-tree code keeps
// using `memory.Block` while plugin authors import pkg/memory directly.
type Block = pubmem.Block

// NewBlock creates a new block with generated ID.
func NewBlock(label, content string, maxChars int) *Block {
	now := time.Now().UTC().Format(time.RFC3339)
	return &Block{
		ID:        generateID(),
		Label:     label,
		Content:   content,
		MaxChars:  maxChars,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// ============================================================================
// CoreMemory - In-Context Block Memory
// ============================================================================

// CoreMemory manages block-based in-context memory with file persistence.
// Inspired by Hermes' memory storage pattern with Frozen Snapshot.
type CoreMemory struct {
	mu     sync.RWMutex
	blocks []*Block

	// Block size limits by label
	limits map[string]int

	// File storage path
	memoryRoot string

	// Frozen snapshot for system prompt - set once at Load() time
	// Mid-session writes update files but do NOT change this snapshot
	snapshot string
}

// Default limits for each block type.
var defaultLimits = map[string]int{
	"user":    2200, // User preferences
	"system":  3000, // System identity
	"working": 4000, // Current task context
	"summary": 1500, // Summarized context
}

// File names for Hermes-style Core Memory persistence.
const (
	FileMemMemory  = "MEMORY.md"  // user block
	FileMemUser    = "USER.md"    // user preferences
	FileMemSystem  = "SYSTEM.md"  // system block
	FileMemWorking = "WORKING.md" // working block
	FileMemSummary = "SUMMARY.md" // summary block
)

// NewCoreMemory creates a new CoreMemory instance.
func NewCoreMemory(memoryRoot string) *CoreMemory {
	cm := &CoreMemory{
		limits:     defaultLimits,
		memoryRoot: memoryRoot,
	}
	// Initialize with default blocks
	for label, limit := range defaultLimits {
		cm.blocks = append(cm.blocks, NewBlock(label, "", limit))
	}
	// Try to load existing memory from files
	cm.Load()
	return cm
}

// GetBlocks returns all blocks.
func (cm *CoreMemory) GetBlocks() []*Block {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.blocks
}

// GetBlock returns block by label.
func (cm *CoreMemory) GetBlock(label string) *Block {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, b := range cm.blocks {
		if b.Label == label {
			return b
		}
	}
	return nil
}

// UpdateBlock updates a block's content, respecting max chars limit.
// Automatically persists to file after update.
//
// Important: also refreshes cm.snapshot so the next BuildContext call
// sees the update. Without this, the Hermes-style "frozen snapshot at
// Load()" semantics meant Memory.add writes would persist on disk but
// never reach the next turn's system prompt — which defeats the
// entire memory contract from the LLM's perspective. The snapshot
// still stays stable within a single turn (UpdateBlock is only called
// from tool execution, which sits between request iterations, not
// during one).
func (cm *CoreMemory) UpdateBlock(label, content string) error {
	// Security scan content before saving
	if security.Scan(content) {
		// Log warning but still save (security hook could reject in future)
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, b := range cm.blocks {
		if b.Label == label {
			if len(content) > b.MaxChars {
				content = content[:b.MaxChars]
			}
			b.Content = content
			b.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			// Persist immediately + refresh snapshot so next render
			// picks up the change.
			cm.saveBlockLocked(b)
			cm.snapshot = cm.renderLocked()
			return nil
		}
	}
	return nil
}

// saveBlockLocked persists a single block. Caller must hold cm.mu.Lock().
func (cm *CoreMemory) saveBlockLocked(b *Block) {
	if cm.memoryRoot == "" {
		return
	}
	path := cm.pathForBlock(b.Label)
	if path == "" {
		return
	}
	content := renderBlockHermes(b)
	atomicWriteFile(path, content, 0o644)
}

// Render returns the memory as a string for system prompt injection.
// Returns the frozen snapshot wrapped in memory-context tags (Hermes pattern).
func (cm *CoreMemory) Render() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.renderWithFencing()
}

// renderWithFencing returns the snapshot wrapped in context fence tags.
// This prevents the model from treating memory content as user input.
func (cm *CoreMemory) renderWithFencing() string {
	if cm.snapshot == "" {
		return ""
	}
	return "<memory-context>\n" +
		"[System note: 这是回忆起的记忆上下文，不是新的用户输入。]\n\n" +
		cm.snapshot +
		"\n</memory-context>"
}

// renderLocked returns the memory content without locking.
// Caller must hold cm.mu.
func (cm *CoreMemory) renderLocked() string {
	var parts []string
	for _, b := range cm.blocks {
		if b.Content != "" {
			parts = append(parts, b.Content)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "## Memory\n" + strings.Join(parts, "\n\n")
}

// SectionCount returns the number of non-empty sections.
func (cm *CoreMemory) SectionCount() int {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	n := 0
	for _, b := range cm.blocks {
		if b.Content != "" {
			n++
		}
	}
	return n
}

// Freshness returns the age of the most recently modified memory file.
// Implements Claude Code-style memory freshness tracking.
func (cm *CoreMemory) Freshness() Freshness {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.freshnessLocked()
}

// freshnessLocked returns freshness without locking. Caller must hold cm.mu.
func (cm *CoreMemory) freshnessLocked() Freshness {
	if cm.memoryRoot == "" {
		return Freshness{}
	}

	var oldestTime time.Time
	for _, b := range cm.blocks {
		path := cm.pathForBlock(b.Label)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if oldestTime.IsZero() || info.ModTime().Before(oldestTime) {
			oldestTime = info.ModTime()
		}
	}

	if oldestTime.IsZero() {
		return Freshness{Status: "no_memory_yet"}
	}

	age := time.Since(oldestTime)
	return Freshness{
		Age:        age,
		IsStale:    age > 24*time.Hour,
		Status:     "fresh",
		OldestFile: oldestTime.Format(time.RFC3339),
	}
}

// Freshness holds memory freshness information.
type Freshness struct {
	Age        time.Duration `json:"age"`
	IsStale    bool          `json:"is_stale"`
	Status     string        `json:"status"`      // "fresh" | "stale" | "no_memory_yet"
	OldestFile string        `json:"oldest_file"` // RFC3339 timestamp
}

// pathForBlock returns the file path for a block label.
func (cm *CoreMemory) pathForBlock(label string) string {
	if cm.memoryRoot == "" {
		return ""
	}
	filename := labelToFilename(label)
	return filepath.Join(cm.memoryRoot, filename)
}

// labelToFilename maps block label to Hermes-style filename.
func labelToFilename(label string) string {
	switch label {
	case "user":
		return FileMemMemory
	case "system":
		return FileMemSystem
	case "working":
		return FileMemWorking
	case "summary":
		return FileMemSummary
	default:
		return strings.ToUpper(label) + ".md"
	}
}

// Load reads Core Memory from files. Non-destructive - only updates content if file exists.
// Captures frozen snapshot for system prompt after loading.
func (cm *CoreMemory) Load() {
	if cm.memoryRoot == "" {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for _, b := range cm.blocks {
		path := cm.pathForBlock(b.Label)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Parse Hermes-style format: strip header, extract content
		content := parseMemoryFile(string(data), b.Label)
		b.Content = content
		b.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	// Capture frozen snapshot for system prompt injection
	cm.snapshot = cm.renderLocked()
}

// GetSnapshot returns the frozen snapshot for system prompt injection.
// This returns the state captured at Load() time, NOT the live state.
// Mid-session writes do not affect this snapshot.
func (cm *CoreMemory) GetSnapshot() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.snapshot
}

// Save persists Core Memory to files using atomic write (temp + rename).
func (cm *CoreMemory) Save() error {
	if cm.memoryRoot == "" {
		return nil
	}
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if err := os.MkdirAll(cm.memoryRoot, 0o755); err != nil {
		return err
	}

	for _, b := range cm.blocks {
		path := cm.pathForBlock(b.Label)
		if path == "" {
			continue
		}
		// Render Hermes-style content
		content := renderBlockHermes(b)
		if err := atomicWriteFile(path, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// parseMemoryFile extracts content from Hermes-style memory file.
// Format: Header lines, then §-separated entries.
func parseMemoryFile(data, label string) string {
	// Find the § separator (Hermes format: entries separated by §)
	parts := strings.Split(data, "§")
	var content []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			content = append(content, p)
		}
	}
	// If no § found, use raw content after stripping header lines
	if len(content) == 0 {
		lines := strings.Split(data, "\n")
		for _, line := range lines {
			// Skip header lines (between ═══ lines)
			if strings.Contains(line, "═") {
				continue
			}
			line = strings.TrimSpace(line)
			if line != "" {
				content = append(content, line)
			}
		}
	}
	return strings.Join(content, "\n")
}

// renderBlockHermes renders a block in Hermes MEMORY.md style.
// Format:
// ════════════════════════════════════════════
// LABEL [pct% — used/limit chars]
// ════════════════════════════════════════════
// Content here
func renderBlockHermes(b *Block) string {
	if b.Content == "" {
		return ""
	}
	used := len(b.Content)
	pct := float64(used) / float64(b.MaxChars) * 100
	header := "═══════════════════════════════════════════════\n"
	header += strings.ToUpper(b.Label) + " [" + strconv.Itoa(int(pct)) + "% — " + strconv.Itoa(used) + "/" + strconv.Itoa(b.MaxChars) + " chars]\n"
	header += "═══════════════════════════════════════════════\n"
	return header + b.Content
}

// Stats returns memory usage statistics for all blocks.
func (cm *CoreMemory) Stats() map[string]BlockStats {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	stats := make(map[string]BlockStats)
	for _, b := range cm.blocks {
		stats[b.Label] = BlockStats{
			Used:  len(b.Content),
			Limit: b.MaxChars,
			Pct:   float64(len(b.Content)) / float64(b.MaxChars) * 100,
		}
	}
	return stats
}

// BlockStats holds usage statistics for a memory block.
type BlockStats struct {
	Used  int     `json:"used"`
	Limit int     `json:"limit"`
	Pct   float64 `json:"pct"`
}

// ============================================================================
// ArchivalMemory - Persistent Long-Term Memory
// ============================================================================

// Passage represents a retrievable memory passage.
//
// Type categorizes the passage's semantic role (claude-code's
// memdir/types.ts pattern). Empty Type is treated as TypeContext for
// backwards compatibility with passages written before this field
// existed.
type Passage struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Type      string    `json:"type,omitempty"` // user | feedback | project | reference | context
	Tags      []string  `json:"tags,omitempty"`
	Embedding []float32 `json:"embedding,omitempty"` // Not used in v1
	CreatedAt string    `json:"created_at"`
}

// Passage type constants — borrowed from claude-code's memdir/types.ts
// classification. Used by the Memory tool, distillation pipeline, and
// retrieval ranking to distinguish "fact about the user" from
// "transient session context."
//
// Semantic guide for callers (and the distillation prompt):
//
//   - TypeUser       — durable identity / preferences ("user prefers Chinese
//     replies", "user is on macOS"). Highest retrieval
//     priority; rarely expires.
//   - TypeFeedback   — corrections and explicit guidance the agent should
//     remember ("don't use git push --force", "always
//     run tests before committing"). High priority.
//   - TypeProject    — current-project context that may go stale ("this
//     repo uses Go 1.26", "running on metis branch foo").
//     Medium priority; eligible for time-decay.
//   - TypeReference  — pointers to external resources ("docs at /Users/...",
//     "K8s cluster info in CLAUDE.md"). Low retrieval
//     priority — surfaced when explicitly asked.
//   - TypeContext    — generic "remember this for now" with no clear
//     classification. Default fallback. Lower priority
//     than the four above so curated types win.
const (
	TypeUser      = "user"
	TypeFeedback  = "feedback"
	TypeProject   = "project"
	TypeReference = "reference"
	TypeContext   = "context"
)

// IsKnownType returns true for the canonical type tags. Unknown values
// from older passages or third-party writes survive (we don't drop them
// on read), but tools / prompts use this to validate new writes.
func IsKnownType(t string) bool {
	switch t {
	case TypeUser, TypeFeedback, TypeProject, TypeReference, TypeContext:
		return true
	}
	return false
}

// ArchivalMemory manages persistent archival memory with file-based storage.
// Stores passages in passages.jsonl and maintains an index.json for fast lookup.
type ArchivalMemory struct {
	mu    sync.RWMutex
	root  string
	index map[string]Passage // in-memory index for fast lookup
}

// NewArchivalMemory creates a new ArchivalMemory instance.
func NewArchivalMemory(root string) (*ArchivalMemory, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	am := &ArchivalMemory{root: root, index: make(map[string]Passage)}
	am.loadIndex()
	return am, nil
}

// loadIndex reads the index file if it exists.
func (am *ArchivalMemory) loadIndex() {
	indexPath := filepath.Join(am.root, "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}
	// Simple line-based index: ID|Timestamp|ContentPreview
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		am.index[parts[0]] = Passage{
			ID:        parts[0],
			CreatedAt: parts[1],
			Content:   parts[2],
		}
	}
}

// saveIndex persists the index to disk.
func (am *ArchivalMemory) saveIndex() error {
	indexPath := filepath.Join(am.root, "index.json")
	var lines []string
	for _, p := range am.index {
		preview := p.Content
		if len(preview) > 200 {
			preview = preview[:200]
		}
		lines = append(lines, p.ID+"|"+p.CreatedAt+"|"+preview)
	}
	content := strings.Join(lines, "\n")
	return atomicWriteFile(indexPath, content, 0o644)
}

// Insert adds a new passage to archival memory.
func (am *ArchivalMemory) Insert(p Passage) error {
	am.mu.Lock()
	defer am.mu.Unlock()

	if p.ID == "" {
		p.ID = generateID()
	}
	if p.CreatedAt == "" {
		p.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	// Update index
	am.index[p.ID] = p

	// Store full passage as JSON lines using proper JSON encoding
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	line := string(data) + "\n"

	// Store full passage as JSON lines
	path := filepath.Join(am.root, "passages.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(line)
	if err != nil {
		return err
	}

	// Update index file
	return am.saveIndex()
}

// SearchOptions contains filtering options for archival search.
type SearchOptions struct {
	Query  string
	Tags   []string // filter by tags
	Types  []string // filter by passage type (TypeUser / TypeFeedback / ...)
	Since  string   // ISO timestamp - only return passages after this time
	Until  string   // ISO timestamp - only return passages before this time
	Limit  int
	SortBy string // "relevance" | "recent" (default: "recent")
}

// Search finds passages matching query with optional filters.
// SortBy semantics:
//   - "relevance": BM25-rank all (filter-passing) passages by query, even
//     ones that don't contain the literal query as a substring.
//   - "recent" or "" (default): return passages newest-first; query acts
//     as a case-insensitive substring filter.
func (am *ArchivalMemory) Search(opts SearchOptions) ([]Passage, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()

	path := filepath.Join(am.root, "passages.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // No passages yet
	}

	useRelevance := opts.SortBy == "relevance" && opts.Query != ""

	var allPassages []Passage
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, err := parsePassageLine(line)
		if err != nil {
			continue
		}

		// Substring filter only applies to non-relevance modes; BM25 handles
		// matching through token overlap and we don't want to throw away
		// passages that share tokens but not the literal query string.
		if !useRelevance && opts.Query != "" &&
			!strings.Contains(strings.ToLower(p.Content), strings.ToLower(opts.Query)) {
			continue
		}

		// Filter by tags
		if len(opts.Tags) > 0 {
			hasTag := false
			for _, t := range opts.Tags {
				for _, pt := range p.Tags {
					if t == pt {
						hasTag = true
						break
					}
				}
			}
			if !hasTag {
				continue
			}
		}

		// Filter by time range
		if opts.Since != "" && p.CreatedAt < opts.Since {
			continue
		}
		if opts.Until != "" && p.CreatedAt > opts.Until {
			continue
		}

		// Filter by passage type. Empty Types in opts = no filter
		// (all types pass). Passages without a Type field are
		// treated as TypeContext for filter purposes — matches the
		// "default fallback" semantics in the Type constants doc.
		if len(opts.Types) > 0 {
			pType := p.Type
			if pType == "" {
				pType = TypeContext
			}
			matched := false
			for _, t := range opts.Types {
				if t == pType {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		allPassages = append(allPassages, p)
	}

	results := allPassages
	if useRelevance && len(allPassages) > 0 {
		// Switched to BM25F (Robertson 2005) — passages have a Content
		// body and a Tags list; tag matches deserve more weight than
		// arbitrary body matches because tags are curated keywords
		// (the user / agent thought enough to attach them). The
		// weight tuning lives in bm25.go (weightTags=2.0,
		// weightContent=1.0) and matches Elasticsearch's
		// long-standing default. Single-tag-less passages score
		// identically to old BM25 — backwards compatible.
		docs := make([]*BM25Doc, 0, len(allPassages))
		pmap := make(map[string]Passage, len(allPassages))
		for _, p := range allPassages {
			docs = append(docs, NewBM25FDoc(p.ID, p.Content, p.Tags))
			pmap[p.ID] = p
		}
		ranked := BM25FRank(opts.Query, docs)
		results = make([]Passage, 0, len(ranked))
		for _, r := range ranked {
			results = append(results, pmap[r.ID])
		}
	} else if opts.SortBy == "recent" || opts.SortBy == "" {
		// Newest-first: passages.jsonl appends so reverse the slice.
		for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
			results[i], results[j] = results[j], results[i]
		}
	}

	if opts.Limit > 0 && len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, nil
}

// tokenize splits text into lowercase words, removing punctuation.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var words []string
	var current strings.Builder
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			current.WriteRune(r)
		} else {
			if current.Len() > 2 { // Skip short words
				words = append(words, current.String())
			}
			current.Reset()
		}
	}
	if current.Len() > 2 {
		words = append(words, current.String())
	}
	return words
}

// parsePassageLine parses a JSON line into a Passage.
func parsePassageLine(line string) (Passage, error) {
	// Use standard JSON decoding
	var p Passage
	err := json.Unmarshal([]byte(line), &p)
	return p, err
}

// Stats returns archival memory statistics.
func (am *ArchivalMemory) Stats() ArchivalStats {
	am.mu.RLock()
	defer am.mu.RUnlock()

	count := len(am.index)
	var oldest, newest string
	for _, p := range am.index {
		if oldest == "" || p.CreatedAt < oldest {
			oldest = p.CreatedAt
		}
		if newest == "" || p.CreatedAt > newest {
			newest = p.CreatedAt
		}
	}
	return ArchivalStats{Count: count, Oldest: oldest, Newest: newest}
}

// ArchivalStats holds archival memory statistics.
type ArchivalStats struct {
	Count  int    `json:"count"`
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
}

// ============================================================================
// RecallMemory - Conversation History with Summarization
// ============================================================================

// RecallMemory manages conversation history with summarization support.
// Persists messages to messages.jsonl and maintains session metadata.
type RecallMemory struct {
	mu       sync.RWMutex
	messages []Message

	root     string
	limit    int       // Max messages before summarization
	sessions []Session // Session history for compression points
}

// Message represents a conversation message in recall memory.
type Message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// Session represents a conversation session for compression tracking.
type Session struct {
	ID        string `json:"id"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	MsgCount  int    `json:"msg_count"`
	Summary   string `json:"summary,omitempty"`
}

// NewRecallMemory creates a new RecallMemory instance.
func NewRecallMemory(root string, limit int) (*RecallMemory, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	rm := &RecallMemory{
		root:     root,
		limit:    limit,
		sessions: []Session{},
	}
	rm.loadMessages()
	rm.loadSessions()
	return rm, nil
}

// loadMessages reads existing messages from disk using JSON format.
func (rm *RecallMemory) loadMessages() {
	path := filepath.Join(rm.root, "messages.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		rm.messages = append(rm.messages, msg)
	}
}

// saveMessages persists messages to disk using JSON format.
func (rm *RecallMemory) saveMessages() error {
	path := filepath.Join(rm.root, "messages.jsonl")
	var lines []string
	for _, m := range rm.messages {
		data, err := json.Marshal(m)
		if err != nil {
			continue
		}
		lines = append(lines, string(data))
	}
	content := strings.Join(lines, "\n") + "\n"
	return atomicWriteFile(path, content, 0o644)
}

// loadSessions reads session metadata from disk using JSON format.
func (rm *RecallMemory) loadSessions() {
	path := filepath.Join(rm.root, "sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Parse JSON array of sessions
	var sessions []Session
	if err := json.Unmarshal(data, &sessions); err != nil {
		return
	}
	rm.sessions = sessions
}

// saveSessions persists session metadata to disk using JSON format.
func (rm *RecallMemory) saveSessions() error {
	path := filepath.Join(rm.root, "sessions.json")
	data, err := json.Marshal(rm.sessions)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, string(data), 0o644)
}

// Add adds a message to recall memory and persists to disk.
func (rm *RecallMemory) Add(role, content string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.messages = append(rm.messages, Message{
		Role:      role,
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	// Persist after each add
	rm.saveMessages()
}

// GetMessages returns all messages.
func (rm *RecallMemory) GetMessages() []Message {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.messages
}

// ShouldSummarize returns true if messages exceed limit.
func (rm *RecallMemory) ShouldSummarize() bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return len(rm.messages) >= rm.limit
}

// Summarize replaces older messages with a summary.
// Records the compression point in session history for recovery.
func (rm *RecallMemory) Summarize(summary string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if len(rm.messages) < 4 {
		return
	}

	// Record session before compression
	session := Session{
		ID:        generateID(),
		StartedAt: rm.messages[0].Timestamp,
		EndedAt:   time.Now().UTC().Format(time.RFC3339),
		MsgCount:  len(rm.messages),
		Summary:   summary,
	}
	rm.sessions = append(rm.sessions, session)

	// Keep last 2 messages, replace older with summary
	kept := rm.messages[len(rm.messages)-2:]
	rm.messages = []Message{
		{Role: "system", Content: "[Earlier conversation summarized]", Timestamp: time.Now().UTC().Format(time.RFC3339)},
		{Role: "system", Content: summary, Timestamp: time.Now().UTC().Format(time.RFC3339)},
	}
	rm.messages = append(rm.messages, kept...)

	// Persist updated state
	rm.saveMessages()
	rm.saveSessions()
}

// Stats returns recall memory statistics.
func (rm *RecallMemory) Stats() RecallStats {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var oldest, newest string
	if len(rm.messages) > 0 {
		oldest = rm.messages[0].Timestamp
		newest = rm.messages[len(rm.messages)-1].Timestamp
	}
	return RecallStats{
		Messages: len(rm.messages),
		Oldest:   oldest,
		Newest:   newest,
		Sessions: len(rm.sessions),
	}
}

// RecallStats holds recall memory statistics.
type RecallStats struct {
	Messages int    `json:"messages"`
	Oldest   string `json:"oldest"`
	Newest   string `json:"newest"`
	Sessions int    `json:"sessions"`
}

// ============================================================================
// MemoryManager - Orchestrates All Memory Types
// ============================================================================

// MemoryManager orchestrates core, archival, and recall memory.
type MemoryManager struct {
	core     *CoreMemory
	archival *ArchivalMemory
	recall   *RecallMemory
	daily    *DailyStore

	root string
}

// NewMemoryManager creates a new MemoryManager.
func NewMemoryManager(root string) (*MemoryManager, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	core, archival, recall, err := loadOrCreate(root)
	if err != nil {
		return nil, err
	}

	daily, err := NewDailyStore(filepath.Join(root, "daily"))
	if err != nil {
		return nil, err
	}

	return &MemoryManager{
		core:     core,
		archival: archival,
		recall:   recall,
		daily:    daily,
		root:     root,
	}, nil
}

func loadOrCreate(root string) (*CoreMemory, *ArchivalMemory, *RecallMemory, error) {
	coreRoot := filepath.Join(root, "core.d")
	if err := os.MkdirAll(coreRoot, 0o755); err != nil {
		return nil, nil, nil, err
	}

	core := NewCoreMemory(coreRoot)

	archival, err := NewArchivalMemory(filepath.Join(root, "archival"))
	if err != nil {
		return nil, nil, nil, err
	}

	recall, err := NewRecallMemory(filepath.Join(root, "recall"), 50)
	if err != nil {
		return nil, nil, nil, err
	}

	return core, archival, recall, nil
}

// Core returns the in-context block-memory tier. Exposed so the
// builtin Memory tool can call UpdateBlock / GetBlock directly,
// keeping a single source of truth for what the LLM sees in
// BuildContext on the next turn. Without this accessor the tool
// wrote to its own ~/.metis/memories/ flat-file path and the
// changes never reached the system prompt — the 2026-04-30 audit's
// critical disconnect bug.
func (mm *MemoryManager) Core() *CoreMemory { return mm.core }

// Archival returns the long-term passage tier. Same rationale as
// Core() — let downstream tools (Memory.add, Memory.search) hit the
// canonical store instead of forking their own file scheme.
func (mm *MemoryManager) Archival() *ArchivalMemory { return mm.archival }

// BuildContext builds the memory context for system prompt injection.
//
// Composition (in order):
//
//  1. Core memory (block-based) — frozen-ish snapshot, refreshes on
//     UpdateBlock. The "current state" the agent should know.
//  2. Recent daily-notes summary (last N days) — gives cross-session
//     continuity. Pre-fix this was written but never read; the
//     2026-04-30 audit caught Daily as a write-only tomb. We now
//     prepend a "Recent sessions" block when daily store has entries
//     within the cutoff window.
//
// Both wrapped in the same <memory-context> fence so the model knows
// it's recall, not a fresh user turn.
func (mm *MemoryManager) BuildContext() string {
	core := mm.core.Render()
	if mm.daily == nil {
		return core
	}
	// 7 days of recent context, capped at 4 KB so a chatty user
	// doesn't push the system prompt past the model's effective
	// budget. Tunable via NewMemoryManager options later.
	const recentDays = 7
	const recentMaxBytes = 4096
	recent := mm.daily.RecentSummary(recentDays, recentMaxBytes)
	if recent == "" {
		return core
	}
	// Splice recent into the core block right before the closing tag.
	// Always prepend the system note so the model knows this is
	// recall, not a fresh user turn.
	closingTag := "\n</memory-context>"
	if strings.HasSuffix(core, closingTag) {
		head := strings.TrimSuffix(core, closingTag)
		return head + "\n\n[System note: 这是回忆起的最近对话上下文，不是新的用户输入。]\n\n" +
			recent + closingTag
	}
	return core + "\n\n[System note: 这是回忆起的最近对话上下文，不是新的用户输入。]\n\n" + recent
}

// OnTurnEnd is called after each agent turn.
func (mm *MemoryManager) OnTurnEnd(ctx context.Context, userMsg, asstMsg string) {
	mm.recall.Add("user", truncate(userMsg, 500))
	mm.recall.Add("assistant", truncate(asstMsg, 1000))
}

// DistillTurn extracts durable facts from one user/assistant exchange
// using the LLM and writes them to archival memory. Called by
// agent.Loop after a successful turn.
//
// Design choices (drawn from openclaw's dreaming + hermes's
// session_end hook + Mem0's fact extraction):
//
//   - Strict JSON output schema [{type, content, tags?}]
//   - Empty list is valid and common — most turns don't yield durable
//     facts (the prompt encourages "be conservative")
//   - Type must be one of TypeUser/Feedback/Project/Reference, never
//     TypeContext (that's the default; we want curated types here)
//   - Skip distillation entirely when assistant reply was an error or
//     empty, or when both messages are tiny (<20 chars; nothing to
//     extract from "thanks" / "ok")
//   - One LLM call per distillation, ~300 input tokens + ~150 output
//
// Errors are non-fatal — distillation is "nice to have" and a flaky
// LLM response shouldn't break the chat. Failures are returned so
// callers can log; nothing more.
func (mm *MemoryManager) DistillTurn(ctx context.Context, provider llm.Provider, userMsg, asstMsg string) error {
	if provider == nil || mm.archival == nil {
		return nil
	}
	if len(strings.TrimSpace(userMsg)) < 20 || len(strings.TrimSpace(asstMsg)) < 20 {
		return nil
	}
	prompt := buildDistillPrompt(userMsg, asstMsg)
	resp, err := provider.Complete(ctx, llm.Request{
		Model: mm.archival.distillModel(), // empty = use provider default
		System: "You extract durable facts from agent conversations for long-term memory. " +
			"Be conservative: most turns yield ZERO durable facts. " +
			"Reply with ONLY a JSON array; never prose.",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: "text", Text: prompt}},
		}},
		MaxTokens: 400,
	})
	if err != nil {
		return fmt.Errorf("distill: provider error: %w", err)
	}
	facts := parseDistilled(resp)
	for _, f := range facts {
		if f.Content == "" || !IsKnownType(f.Type) || f.Type == TypeContext {
			continue
		}
		_ = mm.archival.Insert(Passage{
			Content: f.Content,
			Type:    f.Type,
			Tags:    f.Tags,
		})
	}
	return nil
}

// distillModel returns the model id to use for distillation calls.
// Currently we let the provider default decide (cheaper than forcing
// opus); future work could expose a [memory.distill] config block.
func (am *ArchivalMemory) distillModel() string { return "" }

// distilledFact is the JSON shape we expect from the distillation
// prompt. Tags are optional; an empty slice is fine.
type distilledFact struct {
	Type    string   `json:"type"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// parseDistilled extracts the JSON array from the LLM's response. The
// model often wraps the array in markdown fences (```json ... ```)
// even after we ask for raw JSON, so we tolerate that.
func parseDistilled(resp *llm.Response) []distilledFact {
	if resp == nil {
		return nil
	}
	var raw string
	for _, b := range resp.Content {
		if b.Type == "text" {
			raw += b.Text
		}
	}
	raw = strings.TrimSpace(raw)
	// Strip ```json ... ``` wrapping if present.
	if strings.HasPrefix(raw, "```") {
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	// Find the first [ and last ] so any leading commentary the model
	// snuck in still parses.
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return nil
	}
	body := raw[start : end+1]
	var out []distilledFact
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil
	}
	return out
}

func buildDistillPrompt(userMsg, asstMsg string) string {
	var b strings.Builder
	b.WriteString("Extract any DURABLE facts from this user-assistant exchange that the agent should remember in FUTURE sessions. Reply [] if nothing qualifies.\n\n")
	b.WriteString("Categories (use exactly one per fact):\n")
	b.WriteString("- user: durable identity / preferences (\"user prefers Chinese replies\", \"user is on macOS\")\n")
	b.WriteString("- feedback: corrections / explicit guidance to remember (\"don't use git push --force\", \"always run tests before committing\")\n")
	b.WriteString("- project: current-project context that may go stale (\"this repo uses Go 1.26\", \"running on metis branch foo\")\n")
	b.WriteString("- reference: pointers to external resources (\"docs at /Users/...\", \"K8s cluster info in CLAUDE.md\")\n\n")
	b.WriteString("Be CONSERVATIVE. Most turns yield 0 facts. Skip:\n")
	b.WriteString("- Transient task state (current debugging context — that's working memory, not durable)\n")
	b.WriteString("- Greetings, acknowledgements, polite chatter\n")
	b.WriteString("- Code the agent just wrote (it's in git)\n")
	b.WriteString("- Things derivable from CLAUDE.md / config files\n\n")
	b.WriteString("Output ONLY a JSON array, e.g. [{\"type\":\"user\",\"content\":\"prefers Chinese\"},{\"type\":\"feedback\",\"content\":\"never use git push --force on main\"}]\n\n")
	b.WriteString("---\n\nUser:\n")
	b.WriteString(truncate(userMsg, 1500))
	b.WriteString("\n\nAssistant:\n")
	b.WriteString(truncate(asstMsg, 2000))
	b.WriteString("\n\nFacts JSON:")
	return b.String()
}

// Save persists memory to disk via CoreMemory's atomic save.
func (mm *MemoryManager) Save() error {
	// Delegate to CoreMemory which handles locking and atomic writes
	return mm.core.Save()
}

// Freshness returns the freshness of the oldest memory file across all memory types.
func (mm *MemoryManager) Freshness() Freshness {
	coreFresh := mm.core.Freshness()
	if coreFresh.Status == "no_memory_yet" {
		return coreFresh
	}
	return coreFresh
}

// SaveDailyNote creates a new daily session note.
// Triggered by /new or /reset commands.
func (mm *MemoryManager) SaveDailyNote(sessionID, source, summary string) error {
	if mm.daily == nil {
		return nil
	}
	return mm.daily.Save(sessionID, source, summary)
}

// ListDailyNotes returns recent daily notes sorted by date (newest first).
func (mm *MemoryManager) ListDailyNotes(limit int) ([]DailyNote, error) {
	if mm.daily == nil {
		return nil, nil
	}
	return mm.daily.List(limit)
}

// FindRelevantMemories uses LLM-guided selection to find memories relevant to a query.
// Inspired by Claude Code's findRelevantMemories.ts pattern which uses a separate
// LLM call to select up to N relevant memory files based on semantic similarity.
func (mm *MemoryManager) FindRelevantMemories(ctx context.Context, provider llm.Provider, query string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}

	// Retrieve candidate passages from archival memory
	candidates, err := mm.archival.Search(SearchOptions{Limit: 20, SortBy: "relevance"})
	if err != nil || len(candidates) == 0 {
		// Fallback to core memory if no archival
		return mm.findRelevantCore(ctx, provider, query, limit)
	}

	// Build the rerank prompt — RankGPT-style listwise ordering rather
	// than the older "select top K" formulation. Sun et al 2023 ("Is
	// ChatGPT Good at Search? Investigating Large Language Models as
	// Re-Ranking Agents", EMNLP'23) showed that asking the LLM to
	// produce a *full re-ordering* of the candidate list outperforms
	// a "pick the relevant ones" prompt, because the model has to
	// compare ALL candidates pairwise before committing to an order
	// — pointwise selection lets it skip half the comparisons.
	//
	// We still cap the returned tail at `limit` (top-K is what callers
	// want), but the rerank itself is global. Output format is a JSON
	// array of all N indices in best-first order; we slice to limit
	// after parsing.
	var sb strings.Builder
	sb.WriteString("Re-rank the memory passages below by relevance to the user's query.\n\n")
	sb.WriteString("Query: " + query + "\n\n")
	sb.WriteString("Candidate passages (numbered 1.." + strconv.Itoa(len(candidates)) + "):\n")
	for i, p := range candidates {
		sb.WriteString(strconv.Itoa(i+1) + ". " + p.Content + "\n\n")
	}
	sb.WriteString("Output a JSON array containing ALL candidate indices, ordered from most to least relevant ")
	sb.WriteString("(e.g., [3,1,5,2,4,...] when there are 5 candidates). Include every index exactly once. ")
	sb.WriteString("If a passage is irrelevant, place it at the END of the array — do not omit it. ")
	sb.WriteString("Respond with ONLY the JSON array, no commentary.")

	// Call LLM to rerank
	resp, err := provider.Complete(ctx, llm.Request{
		System:   "You are a listwise reranker for memory retrieval. Output ONLY a JSON array of indices, with every input index present exactly once, ordered most→least relevant.",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: sb.String()}}}},
	})
	if err != nil {
		return nil, err
	}

	// Parse response to extract indices
	indices := parseIndicesFromResponse(resp)
	var relevant []string
	for _, idx := range indices {
		if idx > 0 && idx <= len(candidates) {
			relevant = append(relevant, candidates[idx-1].Content)
			if len(relevant) >= limit {
				break
			}
		}
	}

	// If LLM selection failed, fall back to TF-IDF results
	if len(relevant) == 0 {
		for _, p := range candidates {
			if len(relevant) >= limit {
				break
			}
			relevant = append(relevant, p.Content)
		}
	}

	return relevant, nil
}

// findRelevantCore finds relevant memories from core memory blocks.
func (mm *MemoryManager) findRelevantCore(ctx context.Context, provider llm.Provider, query string, limit int) ([]string, error) {
	blocks := mm.core.GetBlocks()
	var contents []string
	for _, b := range blocks {
		if b.Content != "" {
			contents = append(contents, b.Label+": "+b.Content)
		}
	}

	if len(contents) == 0 {
		return nil, nil
	}

	// Use LLM to select relevant blocks
	var sb strings.Builder
	sb.WriteString("Given the query, select the most relevant memory blocks.\n\n")
	sb.WriteString("Query: " + query + "\n\n")
	sb.WriteString("Memory blocks:\n")
	for i, c := range contents {
		sb.WriteString(strconv.Itoa(i+1) + ". " + c + "\n\n")
	}
	sb.WriteString("Respond with JSON array of indices.")

	resp, err := provider.Complete(ctx, llm.Request{
		System:   "You are a memory relevance selector. Respond with ONLY a JSON array of indices.",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: sb.String()}}}},
	})
	if err != nil {
		// Fallback: return all non-empty blocks
		if len(contents) > limit {
			return contents[:limit], nil
		}
		return contents, nil
	}

	indices := parseIndicesFromResponse(resp)
	var relevant []string
	for _, idx := range indices {
		if idx > 0 && idx <= len(contents) {
			relevant = append(relevant, contents[idx-1])
			if len(relevant) >= limit {
				break
			}
		}
	}

	if len(relevant) == 0 {
		return contents, nil
	}
	return relevant, nil
}

// parseIndicesFromResponse extracts integer indices from LLM JSON response.
func parseIndicesFromResponse(resp *llm.Response) []int {
	if resp == nil || len(resp.Content) == 0 {
		return nil
	}

	var text string
	for _, c := range resp.Content {
		if c.Type == "text" {
			text = c.Text
			break
		}
	}
	if text == "" {
		return nil
	}

	// Try to parse as JSON array
	var indices []int
	if err := json.Unmarshal([]byte(text), &indices); err != nil {
		// Fallback: extract numbers from text
		indices = extractNumbers(text)
	}
	return indices
}

// extractNumbers finds all integers in a string.
func extractNumbers(s string) []int {
	var result []int
	var current strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				if n, err := strconv.Atoi(current.String()); err == nil {
					result = append(result, n)
				}
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		if n, err := strconv.Atoi(current.String()); err == nil {
			result = append(result, n)
		}
	}
	return result
}

// ============================================================================
// Helpers
// ============================================================================

func generateID() string {
	return time.Now().UTC().Format("20060102150405") + "-" + randomString(8)
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		seed := uint64(time.Now().UnixNano())
		for i := range buf {
			buf[i] = letters[seed%uint64(len(letters))]
			seed = seed*1103515245 + 12345
		}
		return string(buf)
	}
	for i := range buf {
		buf[i] = letters[int(buf[i])%len(letters)]
	}
	return string(buf)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// atomicWriteFile writes content to a temp file then atomically renames it.
// Inspired by Hermes' _write_file() pattern.
func atomicWriteFile(path, content string, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Atomic rename (on Unix this is rename(2) which is atomic if src/dst on same fs)
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Set permissions after rename (rename preserves src perms on some systems)
	os.Chmod(path, perm)
	return nil
}
