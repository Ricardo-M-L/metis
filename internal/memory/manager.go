// Package memory implements Metis's multi-tier memory system.
// Inspired by MemGPT's block-based memory and Hermes's pluggable providers.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memdir"
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

	// Workspace-scoped blocks (working and summary) live outside the shared
	// core.d directory. User and system remain global, while task-local state
	// cannot leak between repositories opened by the same Metis installation.
	workspaceRoot string

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
	return newCoreMemory(memoryRoot, "")
}

func newCoreMemory(memoryRoot, workspaceRoot string) *CoreMemory {
	cm := &CoreMemory{
		limits:        defaultLimits,
		memoryRoot:    memoryRoot,
		workspaceRoot: workspaceRoot,
	}
	// Initialize with default blocks
	for _, label := range []string{"user", "system", "working", "summary"} {
		limit := defaultLimits[label]
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
	blocks := make([]*Block, 0, len(cm.blocks))
	for _, block := range cm.blocks {
		blocks = append(blocks, cloneBlock(block))
	}
	return blocks
}

// GetBlock returns block by label.
func (cm *CoreMemory) GetBlock(label string) *Block {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, b := range cm.blocks {
		if b.Label == label {
			return cloneBlock(b)
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
	if cm == nil {
		return ErrCoreBlockNotFound
	}
	if err := validateCoreContent(content); err != nil {
		return err
	}
	update := func() error {
		cm.mu.Lock()
		defer cm.mu.Unlock()
		if err := cm.reloadAuthoritativeLocked(true); err != nil {
			return err
		}
		block := cm.blockLocked(label)
		if block == nil {
			return fmt.Errorf("%w: %s", ErrCoreBlockNotFound, label)
		}
		_, err := cm.persistBlockContentLocked(block, content)
		return err
	}
	if cm.memoryRoot == "" {
		return update()
	}
	return withRepositoryLock(repositoryRootForTier(cm.memoryRoot, "core.d"), update)
}

// saveBlockLocked persists a single block. Caller must hold cm.mu.Lock().
func (cm *CoreMemory) saveBlockLocked(b *Block) error {
	if cm.memoryRoot == "" {
		return nil
	}
	path := cm.pathForBlock(b.Label)
	if path == "" {
		return nil
	}
	content := renderBlockHermes(b)
	return atomicWriteFile(path, content, 0o600)
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
	return filepath.Join(cm.rootForBlock(label), filename)
}

func (cm *CoreMemory) rootForBlock(label string) string {
	if cm.workspaceRoot != "" && (label == "working" || label == "summary") {
		return cm.workspaceRoot
	}
	return cm.memoryRoot
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

	// Construction-time hardening normally sanitizes these files before Load,
	// but embedders can also call Load on a long-lived CoreMemory. Reuse the
	// strict authoritative reader so a post-construction symlink/non-regular
	// replacement is never followed into the prompt snapshot. The method keeps
	// its historical no-error signature; on failure the last known-safe
	// snapshot remains unchanged.
	_ = cm.reloadAuthoritativeLocked(true)
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
	return withRepositoryLock(repositoryRootForTier(cm.memoryRoot, "core.d"), func() error {
		cm.mu.Lock()
		defer cm.mu.Unlock()
		// Core mutations are durable before their API returns. Save is a
		// compatibility synchronization barrier: reload authoritative state
		// instead of rewriting a stale process snapshot over another writer.
		return cm.reloadAuthoritativeLocked(true)
	})
}

// parseMemoryFile extracts content from Hermes-style memory file.
// Format: Header lines, then §-separated entries.
// parseMemoryFile is the inverse of renderBlockHermes — strips
// the Hermes-style decoration (two ═══ separator lines + the
// "LABEL [pct% — used/max chars]" header line in between) and
// returns only the user-supplied content.
//
// 2026-05-15 fix: prior implementation tried to use `§` as a separator,
// but renderBlockHermes never emits `§` — `strings.Split(data, "§")`
// returned a single non-empty part with the whole file body, so the
// `len(content) == 0` fallback (the only branch that actually strips
// `═` lines) was dead code. Effect: the entire on-disk file
// (decoration + header + content) was read back as `block.Content`,
// then the next add wrote a NEW header in front of it, growing the
// file linearly with each Memory.add call AND causing the model to
// either see garbage or, more commonly, see nothing because the
// Frozen Snapshot froze a corrupt render.
//
// Current impl drops ANY decoration line wherever it appears
// (not just the first three lines), so files corrupted by the
// old buggy parser self-heal on the next Save: the next
// UpdateBlock call sees a clean Content + writes a single clean
// header to disk.
func parseMemoryFile(data, label string) string {
	upperLabel := strings.ToUpper(strings.TrimSpace(label))
	var content []string
	for _, line := range strings.Split(data, "\n") {
		trimmed := strings.TrimSpace(line)
		// Drop the ═══ separator decoration (any line containing the
		// box-drawing rune is decoration — content shouldn't have it).
		if strings.ContainsRune(trimmed, '═') {
			continue
		}
		// Drop "LABEL [pct% — used/max chars]" header lines emitted
		// by renderBlockHermes. Pattern: starts with the uppercased
		// block label + " [". We compare against the file's expected
		// label (passed in) so renaming a block (e.g. user → memory)
		// won't accidentally strip content that legitimately starts
		// with a different label name.
		if upperLabel != "" && strings.HasPrefix(trimmed, upperLabel+" [") {
			continue
		}
		// Preserve original line spacing — only drop fully blank lines
		// that surround the (now-removed) header block. Internal blank
		// lines inside content stay.
		if trimmed == "" && len(content) == 0 {
			continue
		}
		content = append(content, line)
	}
	return strings.TrimRight(strings.Join(content, "\n"), "\n\r\t ")
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
	return cm.statsLocked()
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
	ID              string    `json:"id"`
	Content         string    `json:"content"`
	Type            string    `json:"type,omitempty"` // user | feedback | project | reference | context
	Tags            []string  `json:"tags,omitempty"`
	Embedding       []float32 `json:"embedding,omitempty"` // Not used in v1
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at,omitempty"`
	LastUsedAt      string    `json:"last_used_at,omitempty"`
	Source          string    `json:"source,omitempty"`
	SourceSessionID string    `json:"source_session_id,omitempty"`
	SourceMessageID string    `json:"source_message_id,omitempty"`
	Scope           string    `json:"scope,omitempty"`
	Confidence      float64   `json:"confidence,omitempty"`
	UseCount        int       `json:"use_count,omitempty"`
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
	mu           sync.RWMutex
	root         string
	index        map[string]Passage // in-memory index for fast lookup
	defaultScope string             // workspace namespace for unscoped project writes
}

// NewArchivalMemory creates a new ArchivalMemory instance.
func NewArchivalMemory(root string) (*ArchivalMemory, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	am := &ArchivalMemory{root: root, index: make(map[string]Passage)}
	if err := am.reloadPassagesLocked(true); err != nil {
		return nil, err
	}
	return am, nil
}

// reloadPassagesLocked refreshes the authoritative JSONL before a mutation.
// Strict mode prevents a subsequent rewrite from silently discarding an
// unparseable line. Caller must hold am.mu.
func (am *ArchivalMemory) reloadPassagesLocked(strict bool) error {
	passages, err := am.readPassagesLocked(strict)
	if err != nil {
		return err
	}
	index := make(map[string]Passage)
	for _, passage := range passages {
		index[passage.ID] = passage
	}
	am.index = index
	return nil
}

// readPassagesLocked loads the ordered, authoritative JSONL snapshot. A
// malformed line may be ignored by read-only search for backwards
// compatibility, but unsafe model-visible content and unsafe filesystem
// objects always fail the whole read. Callers performing a mutation pass
// strict=true so corruption cannot be silently dropped by a rewrite.
func (am *ArchivalMemory) readPassagesLocked(strict bool) ([]Passage, error) {
	path := filepath.Join(am.root, "passages.jsonl")
	raw, _, err := readAuthoritativeRegularFile(am.root, path, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read archival passages: %w", err)
	}
	passages := make([]Passage, 0)
	for lineNumber, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		passage, parseErr := parsePassageLine(line)
		if parseErr != nil || strings.TrimSpace(passage.ID) == "" {
			if strict {
				if parseErr == nil {
					parseErr = errors.New("empty passage id")
				}
				return nil, fmt.Errorf("parse archival passages line %d: %w", lineNumber+1, parseErr)
			}
			continue
		}
		passage, err = validateLoadedPassage(passage)
		if err != nil {
			return nil, fmt.Errorf("validate archival passages line %d: %w", lineNumber+1, err)
		}
		passages = append(passages, passage)
	}
	return passages, nil
}

// validateLoadedPassage mirrors Insert's trust-boundary checks. In
// particular, a single credential is redacted before indexing, while an env
// dump or prompt-injection payload fails closed instead of becoming recalled
// model context.
func validateLoadedPassage(p Passage) (Passage, error) {
	metadata := []string{p.ID, p.Type, p.Source, p.SourceSessionID, p.SourceMessageID, p.Scope}
	metadata = append(metadata, p.Tags...)
	if err := validatePersistedMetadata(metadata...); err != nil {
		return Passage{}, err
	}
	content, err := sanitizeLoadedMemoryContent(p.Content)
	if err != nil {
		return Passage{}, err
	}
	p.Content = content
	return p, nil
}

func sanitizeLoadedMemoryContent(content string) (string, error) {
	redacted := memdir.Redact(content)
	if redacted.Reject {
		return "", ErrSensitiveMemory
	}
	content = redacted.Redacted
	if threats := security.ScanAll(content); hasBlockingThreat(threats) {
		return "", fmt.Errorf("%w: %s", ErrUnsafeMemory, threatKinds(threats))
	}
	return content, nil
}

// saveIndex persists the index to disk.
func (am *ArchivalMemory) saveIndex() error {
	indexPath := filepath.Join(am.root, "index.json")
	var lines []string
	for _, p := range am.index {
		preview := p.Content
		if len(preview) > 200 {
			preview = truncate(preview, 200)
		}
		preview = strings.NewReplacer("\n", " ", "\r", " ", "|", " ").Replace(preview)
		lines = append(lines, p.ID+"|"+p.CreatedAt+"|"+preview)
	}
	sort.Strings(lines)
	content := strings.Join(lines, "\n")
	return atomicWriteFile(indexPath, content, 0o600)
}

// Insert adds a new passage to archival memory.
func (am *ArchivalMemory) Insert(p Passage) error {
	if am == nil {
		return nil
	}
	p.Scope = scopedMemoryType(p.Type, p.Scope, am.defaultScope)
	metadata := []string{p.ID, p.Type, p.Source, p.SourceSessionID, p.SourceMessageID, p.Scope}
	metadata = append(metadata, p.Tags...)
	if err := validatePersistedMetadata(metadata...); err != nil {
		return err
	}
	redacted := memdir.Redact(p.Content)
	if redacted.Reject {
		return ErrSensitiveMemory
	}
	p.Content = redacted.Redacted
	if threats := security.ScanAll(p.Content); hasBlockingThreat(threats) {
		return fmt.Errorf("%w: %s", ErrUnsafeMemory, threatKinds(threats))
	}
	repositoryRoot := repositoryRootForTier(am.root, "archival")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := rejectDeletedSessionLocked(repositoryRoot, p.SourceSessionID); err != nil {
			return err
		}
		am.mu.Lock()
		defer am.mu.Unlock()
		if err := am.reloadPassagesLocked(true); err != nil {
			return err
		}
		return am.insertLocked(p)
	})
}

func (am *ArchivalMemory) insertLocked(p Passage) error {

	if p.ID == "" {
		p.ID = generateID()
	}
	if _, exists := am.index[p.ID]; exists {
		// passages.jsonl is authoritative. Retrying an insert after an
		// index-write failure should repair the compact index without
		// appending a duplicate passage.
		return am.saveIndex()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if p.CreatedAt == "" {
		p.CreatedAt = now
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = p.CreatedAt
	}
	if p.Confidence == 0 {
		p.Confidence = 1
	}

	// Store full passage as JSON lines using proper JSON encoding
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	line := string(data) + "\n"

	// Store full passage as JSON lines
	path := filepath.Join(am.root, "passages.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}

	if _, err = f.WriteString(line); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	am.index[p.ID] = p

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
	if am == nil {
		return nil, nil
	}
	am.mu.Lock()
	allPassages, err := am.readPassagesLocked(false)
	if err != nil {
		am.mu.Unlock()
		return nil, err
	}
	refreshed := make(map[string]Passage, len(allPassages))
	for _, passage := range allPassages {
		refreshed[passage.ID] = passage
	}
	am.index = refreshed
	am.mu.Unlock()

	useRelevance := opts.SortBy == "relevance" && opts.Query != ""
	filteredPassages := make([]Passage, 0, len(allPassages))
	for _, p := range allPassages {
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

		filteredPassages = append(filteredPassages, p)
	}

	results := filteredPassages
	if useRelevance && len(filteredPassages) > 0 {
		// Switched to BM25F (Robertson 2005) — passages have a Content
		// body and a Tags list; tag matches deserve more weight than
		// arbitrary body matches because tags are curated keywords
		// (the user / agent thought enough to attach them). The
		// weight tuning lives in bm25.go (weightTags=2.0,
		// weightContent=1.0) and matches Elasticsearch's
		// long-standing default. Single-tag-less passages score
		// identically to old BM25 — backwards compatible.
		docs := make([]*BM25Doc, 0, len(filteredPassages))
		pmap := make(map[string]Passage, len(filteredPassages))
		for _, p := range filteredPassages {
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

// tokenize splits text into BM25 tokens. Mixed-script aware (2026-05-15):
//
//   - ASCII letters/digits accumulate into whole words (3+ chars). The
//     length floor matches the original behavior; "go" and "ai" still
//     get dropped to keep IDF stable on a corpus that historically
//     never indexed 2-letter Latin tokens.
//
//   - CJK runes (Han / Kana / Hangul) emit BOTH unigrams AND bigrams.
//     Bigrams give precision (a "我喜欢" doc shares "我喜" + "喜欢"
//     bigrams with the query "我喜欢喵咪") while unigrams ensure recall
//     when query and doc only share single chars (query "猫" still hits
//     doc "宠物猫"). BM25's IDF naturally damps the very common chars
//     (的/是/了) so unigram noise stays bounded.
//
//   - Everything else (whitespace, punctuation including CJK
//     punctuation U+3000-U+303F) just flushes the active buffers.
//
// Pre-fix: ASCII-only ([a-z0-9]+ with len>2). Chinese / Japanese /
// Korean queries returned zero tokens so BM25 produced an empty
// hit list — AutoRetrieve effectively disabled for non-Latin users.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var words []string
	var ascii strings.Builder
	var cjk []rune

	flushAscii := func() {
		if ascii.Len() > 2 {
			words = append(words, ascii.String())
		}
		ascii.Reset()
	}
	flushCJK := func() {
		// Single CJK char → unigram only (no bigram possible).
		// Multiple → emit each as unigram + each adjacent pair as bigram.
		for _, r := range cjk {
			words = append(words, string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			words = append(words, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}

	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			flushCJK()
			ascii.WriteRune(r)
		case isCJKRune(r):
			flushAscii()
			cjk = append(cjk, r)
		default:
			flushAscii()
			flushCJK()
		}
	}
	flushAscii()
	flushCJK()
	return words
}

// isCJKRune classifies r as a CJK ideograph or kana / hangul syllable.
// Punctuation in U+3000-U+303F (the CJK Symbols block — `「」、。`)
// is intentionally excluded: punctuation is a delimiter, not signal,
// and indexing it as a token would inflate IDF for noise.
func isCJKRune(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs (most Han chars)
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK Extension A
		return true
	case r >= 0x20000 && r <= 0x2A6DF: // CJK Extension B (rare ideographs)
		return true
	case r >= 0x3040 && r <= 0x309F: // Hiragana
		return true
	case r >= 0x30A0 && r <= 0x30FF: // Katakana
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul Syllables
		return true
	}
	return false
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
	Role            string `json:"role"`
	Content         string `json:"content"`
	Timestamp       string `json:"timestamp"`
	SessionID       string `json:"session_id,omitempty"`
	SourceMessageID string `json:"source_message_id,omitempty"`
	Scope           string `json:"scope,omitempty"`
}

// Session represents a conversation session for compression tracking.
type Session struct {
	ID              string `json:"id"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at"`
	MsgCount        int    `json:"msg_count"`
	Summary         string `json:"summary,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty"`
	SourceMessageID string `json:"source_message_id,omitempty"`
	Scope           string `json:"scope,omitempty"`
}

// NewRecallMemory creates a new RecallMemory instance.
func NewRecallMemory(root string, limit int) (*RecallMemory, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	rm := &RecallMemory{
		root:     root,
		limit:    limit,
		sessions: []Session{},
	}
	rm.loadMessages()
	// messages.jsonl is authoritative. sessions.json is a derived index and
	// may be stale after a crash between the two atomic renames, so never let
	// it overwrite the metadata rebuilt from messages on startup.
	return rm, nil
}

// loadMessages reads existing messages from disk using JSON format.
func (rm *RecallMemory) loadMessages() {
	_ = rm.reloadMessagesLocked(false)
}

// reloadMessagesLocked refreshes the authoritative JSONL snapshot before a
// read-modify-write. Strict mode refuses to rewrite a file containing a
// malformed line, because silently skipping it would turn corruption into
// permanent data loss on the next atomic save. Caller must hold rm.mu.
func (rm *RecallMemory) reloadMessagesLocked(strict bool) error {
	path := filepath.Join(rm.root, "messages.jsonl")
	data, _, err := readAuthoritativeRegularFile(rm.root, path, 0)
	if errors.Is(err, os.ErrNotExist) {
		rm.messages = nil
		rm.sessions = nil
		return nil
	}
	if err != nil {
		return err
	}
	messages := make([]Message, 0)
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			if strict {
				return fmt.Errorf("parse recall messages line %d: %w", lineNumber+1, err)
			}
			continue
		}
		messages = append(messages, msg)
	}
	rm.messages = messages
	rm.sessions = sessionsFromMessages(messages)
	return nil
}

func sessionsFromMessages(messages []Message) []Session {
	var sessions []Session
	byID := make(map[string]int)
	for _, message := range messages {
		id := strings.TrimSpace(message.SessionID)
		if id == "" {
			continue
		}
		index, ok := byID[id]
		if !ok {
			index = len(sessions)
			byID[id] = index
			sessions = append(sessions, Session{
				ID:              id,
				StartedAt:       message.Timestamp,
				SourceSessionID: id,
			})
		}
		session := &sessions[index]
		session.EndedAt = message.Timestamp
		session.MsgCount++
		session.SourceMessageID = message.SourceMessageID
		session.Scope = message.Scope
	}
	return sessions
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
	return atomicWriteFile(path, content, 0o600)
}

// loadSessions reads session metadata from disk using JSON format.
func (rm *RecallMemory) loadSessions() {
	path := filepath.Join(rm.root, "sessions.json")
	data, _, err := readAuthoritativeRegularFile(rm.root, path, 0)
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
	return atomicWriteFile(path, string(data), 0o600)
}

// Add adds a message to recall memory and persists to disk.
func (rm *RecallMemory) Add(role, content string) {
	_ = rm.AddWithMetadata(role, content, "", "", "")
}

// AddWithMetadata persists a recall message with its source identity.
func (rm *RecallMemory) AddWithMetadata(role, content, sessionID, sourceMessageID, scope string) error {
	if err := validatePersistedMetadata(role, sessionID, sourceMessageID, scope); err != nil {
		return err
	}
	content, err := sanitizePersistedText(content)
	if err != nil {
		return err
	}
	repositoryRoot := repositoryRootForTier(rm.root, "recall")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := rejectDeletedSessionLocked(repositoryRoot, sessionID); err != nil {
			return err
		}
		rm.mu.Lock()
		defer rm.mu.Unlock()
		if err := rm.reloadMessagesLocked(true); err != nil {
			return err
		}
		return rm.addWithMetadataLocked(role, content, sessionID, sourceMessageID, scope)
	})
}

func (rm *RecallMemory) addWithMetadataLocked(role, content, sessionID, sourceMessageID, scope string) error {
	oldMessages := append([]Message(nil), rm.messages...)
	oldSessions := append([]Session(nil), rm.sessions...)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rm.messages = append(rm.messages, Message{
		Role:            role,
		Content:         content,
		Timestamp:       now,
		SessionID:       sessionID,
		SourceMessageID: sourceMessageID,
		Scope:           scope,
	})
	rm.recordSessionLocked(sessionID, sourceMessageID, scope, now, 1)
	if err := rm.saveMessages(); err != nil {
		rm.messages = oldMessages
		rm.sessions = oldSessions
		return err
	}
	if err := rm.saveSessions(); err != nil {
		rm.messages = oldMessages
		rm.sessions = oldSessions
		return errors.Join(err, rm.saveMessages())
	}
	return nil
}

// AddTurn persists both sides of a completed exchange in one atomic rewrite.
func (rm *RecallMemory) AddTurn(userContent, assistantContent, sessionID, sourceMessageID, scope string) error {
	if err := validatePersistedMetadata(sessionID, sourceMessageID, scope); err != nil {
		return err
	}
	userContent, err := sanitizePersistedText(userContent)
	if err != nil {
		return err
	}
	assistantContent, err = sanitizePersistedText(assistantContent)
	if err != nil {
		return err
	}
	repositoryRoot := repositoryRootForTier(rm.root, "recall")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := rejectDeletedSessionLocked(repositoryRoot, sessionID); err != nil {
			return err
		}
		rm.mu.Lock()
		defer rm.mu.Unlock()
		if err := rm.reloadMessagesLocked(true); err != nil {
			return err
		}
		return rm.addTurnLocked(userContent, assistantContent, sessionID, sourceMessageID, scope)
	})
}

func (rm *RecallMemory) addTurnLocked(userContent, assistantContent, sessionID, sourceMessageID, scope string) error {
	oldMessages := append([]Message(nil), rm.messages...)
	oldSessions := append([]Session(nil), rm.sessions...)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rm.messages = append(rm.messages,
		Message{Role: "user", Content: userContent, Timestamp: now, SessionID: sessionID, SourceMessageID: sourceMessageID, Scope: scope},
		Message{Role: "assistant", Content: assistantContent, Timestamp: now, SessionID: sessionID, SourceMessageID: sourceMessageID, Scope: scope},
	)
	rm.recordSessionLocked(sessionID, sourceMessageID, scope, now, 2)
	if err := rm.saveMessages(); err != nil {
		rm.messages = oldMessages
		rm.sessions = oldSessions
		return err
	}
	if err := rm.saveSessions(); err != nil {
		rm.messages = oldMessages
		rm.sessions = oldSessions
		return errors.Join(err, rm.saveMessages())
	}
	return nil
}

func (rm *RecallMemory) recordSessionLocked(sessionID, sourceMessageID, scope, now string, messageCount int) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	for i := range rm.sessions {
		if rm.sessions[i].ID != sessionID {
			continue
		}
		rm.sessions[i].EndedAt = now
		rm.sessions[i].MsgCount += messageCount
		rm.sessions[i].SourceSessionID = sessionID
		rm.sessions[i].SourceMessageID = sourceMessageID
		rm.sessions[i].Scope = scope
		return
	}
	rm.sessions = append(rm.sessions, Session{
		ID:              sessionID,
		StartedAt:       now,
		EndedAt:         now,
		MsgCount:        messageCount,
		SourceSessionID: sessionID,
		SourceMessageID: sourceMessageID,
		Scope:           scope,
	})
}

// GetMessages returns all messages.
func (rm *RecallMemory) GetMessages() []Message {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return append([]Message(nil), rm.messages...)
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

	root           string
	workspaceScope string

	usageMu        sync.Mutex
	cache          repositoryCache
	contextCache   contextSnapshotCache
	retrievalCache retrievalSnapshotCache
}

// NewMemoryManager creates a new MemoryManager.
func NewMemoryManager(root string) (*MemoryManager, error) {
	return newMemoryManager(root, "")
}

// NewMemoryManagerForWorkspace returns a view of the canonical repository
// whose project/context retrieval is isolated to workspacePath. User and
// feedback memory remains shared across workspaces.
func NewMemoryManagerForWorkspace(root, workspacePath string) (*MemoryManager, error) {
	scope, err := WorkspaceScope(workspacePath)
	if err != nil {
		return nil, err
	}
	return newMemoryManager(root, scope)
}

func newMemoryManager(root, workspaceScope string) (*MemoryManager, error) {
	// Every constructor is a model-input trust boundary. Runtime startup usually
	// migrates legacy roots first, but tests, plugins, and embedded callers may
	// construct a manager directly. Harden before any tier loader reads disk so
	// unsafe legacy content can never enter the prompt or retrieval index.
	if err := HardenCanonicalRoot(root); err != nil {
		return nil, err
	}

	core, archival, recall, err := loadOrCreate(root, workspaceScope)
	if err != nil {
		return nil, err
	}

	daily, err := NewDailyStore(filepath.Join(root, "daily"))
	if err != nil {
		return nil, err
	}

	mm := &MemoryManager{
		core:           core,
		archival:       archival,
		recall:         recall,
		daily:          daily,
		root:           root,
		workspaceScope: workspaceScope,
	}
	archival.defaultScope = workspaceScope
	if err := mm.importLegacyStoreJSONL(); err != nil {
		return nil, err
	}
	if err := ensurePrivateTree(root); err != nil {
		return nil, err
	}
	return mm, nil
}

func loadOrCreate(root, workspaceScope string) (*CoreMemory, *ArchivalMemory, *RecallMemory, error) {
	coreRoot := filepath.Join(root, "core.d")
	if err := os.MkdirAll(coreRoot, 0o700); err != nil {
		return nil, nil, nil, err
	}
	workspaceCoreRoot := ""
	if workspaceScope != "" {
		workspaceCoreRoot = filepath.Join(root, "workspaces", workspaceID(workspaceScope), "core.d")
		if err := os.MkdirAll(workspaceCoreRoot, 0o700); err != nil {
			return nil, nil, nil, err
		}
	}

	core := newCoreMemory(coreRoot, workspaceCoreRoot)

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

// Core returns the in-context block-memory tier for compatibility and
// read-only views. New read-modify-write callers must use ReadCoreBlock,
// AddCoreBlock, ReplaceCoreBlock, and RemoveCoreBlock so the repository lock
// spans the authoritative reload and atomic write.
func (mm *MemoryManager) Core() *CoreMemory {
	if mm == nil {
		return nil
	}
	return mm.core
}

// Root returns the canonical repository root. Auto-memory and Dream must use
// this exact path instead of independently resolving ~/.metis/memory.
func (mm *MemoryManager) Root() string {
	if mm == nil {
		return ""
	}
	return mm.root
}

// Archival returns the long-term passage tier. Same rationale as
// Core() — let downstream tools (Memory.add, Memory.search) hit the
// canonical store instead of forking their own file scheme.
func (mm *MemoryManager) Archival() *ArchivalMemory {
	if mm == nil {
		return nil
	}
	return mm.archival
}

// AutoRetrieve runs BM25 against the unified archival + topic corpus and returns the top-K
// passages as a single <auto-retrieve>-wrapped string, ready to splice
// into the system prompt. Empty query, empty archival, k<=0, or any
// search error returns "" so the caller can unconditionally append.
//
// Designed to be called once per turn (see Loop.buildRequest):
// retrieval is local-only (no LLM call), BM25 over a few hundred
// passages takes <10ms, so per-turn cost is negligible.
//
// Top-K passages are joined as a numbered list. Content is left
// verbatim (truncating risks corrupting the meaning); callers should
// cap K, not per-passage bytes.
func (mm *MemoryManager) AutoRetrieve(query string, k int) string {
	if mm == nil {
		return ""
	}
	hits := mm.AutoRetrieveCandidates(query, k)
	_ = mm.MarkRetrieved(hits)
	return FormatRetrieveSection(hits)
}

// PreviewAutoRetrieve formats the same BM25 selection as AutoRetrieve without
// recording it as used. Token/context estimators must call this method because
// their hypothetical prompt is never shown to a model.
func (mm *MemoryManager) PreviewAutoRetrieve(query string, k int) string {
	if mm == nil {
		return ""
	}
	return FormatRetrieveSection(mm.AutoRetrieveCandidates(query, k))
}

// AutoRetrieveCandidates is the raw-passage variant of AutoRetrieve:
// returns the BM25 top-N as []Passage so callers (e.g. the agent
// loop's optional LLM rerank path) can post-process before formatting.
//
// Returns nil — not an error — when archival is empty / disabled /
// query is whitespace, mirroring AutoRetrieve's "fail soft" contract.
// Caller can unconditionally pass the result to FormatRetrieveSection.
func (mm *MemoryManager) AutoRetrieveCandidates(query string, k int) []Passage {
	if mm == nil {
		return nil
	}
	return mm.SearchCandidates(query, k)
}

// FormatRetrieveSection wraps a passage list in the same
// <auto-retrieve>-fenced block that AutoRetrieve emits. Exposed so
// LLM-reranked / externally-filtered candidate lists can produce the
// same on-wire shape without duplicating the formatting code.
//
// nil / empty input returns "" so the caller can unconditionally
// concatenate the result without an extra branch.
func FormatRetrieveSection(passages []Passage) string {
	if len(passages) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<auto-retrieve>\n")
	sb.WriteString("[System note: 这些是从 archival memory 中按相关度自动召回的过往片段，仅供参考。]\n")
	for i, p := range passages {
		fmt.Fprintf(&sb, "\n[%d] %s\n", i+1, strings.TrimSpace(p.Content))
	}
	sb.WriteString("</auto-retrieve>")
	return sb.String()
}

// BuildContext builds the memory context for system prompt injection.
//
// Composition is deliberately compact and stable: Core blocks plus the short
// topic manifest. Full topic/archive bodies and session history are not copied
// into every system prompt; BM25 selects the relevant Top-K after the user
// query arrives and the Loop appends that dynamic recall at the request tail.
// This preserves cross-session recall without invalidating the stable prompt
// cache prefix on every turn.
func (mm *MemoryManager) BuildContext() string {
	return mm.buildContextCached()
}

// OnTurnEnd is called after each agent turn.
func (mm *MemoryManager) OnTurnEnd(ctx context.Context, userMsg, asstMsg string) {
	_ = mm.RecordTurn(ctx, "", "", userMsg, asstMsg)
}

// RecordTurn is the metadata-aware replacement for OnTurnEnd. It is
// synchronous because a successful response must be durable before Desktop or
// CLI switches sessions.
func (mm *MemoryManager) RecordTurn(_ context.Context, sessionID, sourceMessageID, userMsg, asstMsg string) error {
	if mm == nil || mm.recall == nil {
		return nil
	}
	userMsg, err := sanitizePersistedText(userMsg)
	if err != nil {
		return err
	}
	asstMsg, err = sanitizePersistedText(asstMsg)
	if err != nil {
		return err
	}
	return mm.recall.AddTurn(truncate(userMsg, 500), truncate(asstMsg, 1000), sessionID, sourceMessageID, "session")
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
	if mm == nil {
		return nil
	}
	return mm.DistillTurnWithMetadata(ctx, provider, "", "", userMsg, asstMsg)
}

// DistillTurnWithMetadata is the provenance-aware distillation entry point.
// DistillTurn remains as a compatibility wrapper for older embedders.
func (mm *MemoryManager) DistillTurnWithMetadata(ctx context.Context, provider llm.Provider, sessionID, sourceMessageID, userMsg, asstMsg string) error {
	if mm == nil || provider == nil || mm.archival == nil {
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
	var errs []error
	for _, f := range facts {
		if f.Content == "" || !IsKnownType(f.Type) || f.Type == TypeContext {
			continue
		}
		if err := mm.archival.Insert(Passage{
			Content:         f.Content,
			Type:            f.Type,
			Tags:            f.Tags,
			Source:          "distillation",
			SourceSessionID: strings.TrimSpace(sessionID),
			SourceMessageID: strings.TrimSpace(sourceMessageID),
			Scope:           scopedMemoryType(f.Type, "", mm.workspaceScope),
		}); err != nil {
			errs = append(errs, err)
			if errors.Is(err, ErrSessionDeleted) {
				break
			}
		}
	}
	return errors.Join(errs...)
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
	if mm == nil || mm.core == nil {
		return nil
	}
	// Delegate to CoreMemory which handles locking and atomic writes
	return mm.core.Save()
}

// Freshness returns the freshness of the oldest memory file across all memory types.
func (mm *MemoryManager) Freshness() Freshness {
	if mm == nil || mm.core == nil {
		return Freshness{Status: "no_memory_yet"}
	}
	coreFresh := mm.core.Freshness()
	if coreFresh.Status == "no_memory_yet" {
		return coreFresh
	}
	return coreFresh
}

// SaveDailyNote creates a new daily session note.
// Triggered by /new or /reset commands.
func (mm *MemoryManager) SaveDailyNote(sessionID, source, summary string) error {
	if mm == nil || mm.daily == nil {
		return nil
	}
	return mm.daily.Save(sessionID, source, summary)
}

// ListDailyNotes returns recent daily notes sorted by date (newest first).
func (mm *MemoryManager) ListDailyNotes(limit int) ([]DailyNote, error) {
	if mm == nil || mm.daily == nil {
		return nil, nil
	}
	return mm.daily.List(limit)
}

// FindRelevantMemories uses LLM-guided selection to find memories relevant to a query.
// Inspired by Claude Code's findRelevantMemories.ts pattern which uses a separate
// LLM call to select up to N relevant memory files based on semantic similarity.
func (mm *MemoryManager) FindRelevantMemories(ctx context.Context, provider llm.Provider, query string, limit int) ([]string, error) {
	if mm == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	// Retrieve candidate passages from the same cached archival + topic
	// corpus used by per-turn AutoRetrieve.
	candidates := mm.AutoRetrieveCandidates(query, 20)
	if len(candidates) == 0 {
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
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	// Limits are byte-oriented for provider/persistence budgeting, but a raw
	// byte slice can split a multi-byte Chinese/Japanese/Korean rune and leave
	// invalid UTF-8 in recall JSON or a distillation prompt.
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// openAuthoritativeRoot opens and pins a canonical memory tier directory.
// Lstat before and after OpenRoot prevents a path that was replaced by a
// symlink (or another inode) from becoming the authority for the read.
func openAuthoritativeRoot(root string) (*os.Root, string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, "", errors.New("memory: empty authoritative root")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, "", err
	}
	before, err := os.Lstat(absRoot)
	if err != nil {
		return nil, "", err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("memory: authoritative root is a symlink: %s", absRoot)
	}
	if !before.IsDir() {
		return nil, "", fmt.Errorf("memory: authoritative root is not a directory: %s", absRoot)
	}
	handle, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, "", err
	}
	after, err := handle.Lstat(".")
	if err != nil {
		handle.Close()
		return nil, "", err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		handle.Close()
		return nil, "", fmt.Errorf("memory: authoritative root changed while opening: %s", absRoot)
	}
	return handle, absRoot, nil
}

// readAuthoritativeRegularFile reads one file without following any symlink
// below the pinned tier root. Intermediate components must be directories and
// the leaf must remain the same regular inode from Lstat through Open.
func readAuthoritativeRegularFile(root, path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	handle, absRoot, err := openAuthoritativeRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer handle.Close()

	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, nil, err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil || rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		if err == nil {
			err = fmt.Errorf("memory: authoritative path escapes root: %s", absPath)
		}
		return nil, nil, err
	}

	parts := strings.Split(rel, string(os.PathSeparator))
	current := ""
	var leafInfo os.FileInfo
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, nil, fmt.Errorf("memory: invalid authoritative path component %q", part)
		}
		current = filepath.Join(current, part)
		info, statErr := handle.Lstat(current)
		if statErr != nil {
			return nil, nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("memory: authoritative path contains symlink: %s", absPath)
		}
		if index < len(parts)-1 {
			if !info.IsDir() {
				return nil, nil, fmt.Errorf("memory: authoritative parent is not a directory: %s", absPath)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("memory: authoritative file is not regular: %s", absPath)
		}
		leafInfo = info
	}
	if maxBytes > 0 && leafInfo.Size() > maxBytes {
		return nil, nil, fmt.Errorf("memory: authoritative file exceeds %d bytes: %s", maxBytes, absPath)
	}

	file, err := handle.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(leafInfo, openedInfo) {
		return nil, nil, fmt.Errorf("memory: authoritative file changed while opening: %s", absPath)
	}

	var reader io.Reader = file
	if maxBytes > 0 {
		reader = io.LimitReader(file, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("memory: authoritative file exceeds %d bytes: %s", maxBytes, absPath)
	}
	return data, openedInfo, nil
}

// atomicWriteFile writes content to a temp file then atomically renames it.
// Inspired by Hermes' _write_file() pattern.
func atomicWriteFile(path, content string, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Apply the final private mode before any bytes are written. Rename keeps
	// the temp inode, so there is no window where the destination exists with
	// CreateTemp's platform-dependent permissions.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
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

	// Defensively normalize an existing target on platforms with unusual
	// rename semantics. Privacy was already guaranteed by chmod-before-write.
	return os.Chmod(path, perm)
}
