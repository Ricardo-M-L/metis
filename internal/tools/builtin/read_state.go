package builtin

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// readStatePathKey folds lexical and stable symlink aliases onto one session
// state key. Callers compute it once per operation and reuse it for Get/Record
// so a path swap cannot make those two steps resolve different targets.
func readStatePathKey(path string) string {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		absPath = filepath.Clean(path)
	}
	current := absPath
	var missing []string
	for {
		if resolved, resolveErr := filepath.EvalSymlinks(current); resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	return filepath.Clean(absPath)
}

// ReadFileState records, per session, when a file was last seen by a
// reading tool (Read, NotebookEdit's read pass) so subsequent writing
// tools (Edit, Write, NotebookEdit) can detect that:
//
//  1. the on-disk contents drifted between the model's mental snapshot
//     and the write (mtime / hash check), or
//  2. the snapshot itself was partial (offset/limit truncated the
//     view). Full-file overwrite under (2) is unsafe; an exact targeted
//     replacement remains safe when the whole-file hash still matches.
//
// Mirrors claude-code's readFileState in tools/FileEditTool/utils.ts
// + fileTracker.ts. The check sits between the staleness Read
// (mtime ⇒ unchanged) and the actual write — see Edit.Execute and
// Write.Execute below.
//
// LRU bound: ReadStateMaxEntries (default 100). Long sessions touching
// thousands of files would otherwise grow this map without bound;
// claude-code's fileTracker uses the same size cap. Eviction is
// least-recently-recorded, so files in the active edit hot path stay
// resident.
//
// Concurrency: Edit / Write are ConcurrencyExclusive so they serialize
// after the parallel Read / Grep batch in dispatch.go. The Mutex
// here only guards multi-session reuse of the same store; intra-batch
// the dispatch tier ordering already prevents read-then-edit races.
type ReadFileState struct {
	mu           sync.Mutex
	entries      map[string]*list.Element
	order        *list.List // front = most recently recorded
	nextRevision uint64
}

// ReadStateMaxEntries caps the number of files tracked at once.
// 100 matches claude-code fileTracker; longer histories would inflate
// memory without obvious benefit since edits cluster around a small
// working set.
const ReadStateMaxEntries = 100

// ReadEntry is the snapshot a Read tool captures.
type ReadEntry struct {
	// Revision binds a model-visible result to this exact read, including
	// repeated reads of unchanged bytes. Old batch receipts cannot revoke or
	// grant the authority of a later read.
	Revision uint64
	// MTime is the file's modification time at read.
	MTime time.Time
	// Hash is sha256(content) — used as a fallback when an OS-level
	// mtime change happens without content change (Windows cloud sync,
	// touch). claude-code FileEditTool.ts:455-466 explains the
	// rationale.
	Hash string
	// ReadAt is the wall-clock time we recorded the read. Used by
	// expiration policies (currently none — entries live the full
	// session). Stays here so it can drive future GC.
	ReadAt time.Time
	// Size is the file's byte length at read time. Recorded so logs /
	// future cache decisions can size the entry without re-statting.
	Size int64
	// Offset / Limit / IsPartialView record how Read sliced the file.
	// IsPartialView fires Write's "re-Read first" guard; Edit still uses Hash.
	Offset        int
	Limit         int
	IsPartialView bool
}

// readNode is the LRU list payload — pairs a path with its entry so
// the back-pointer in the list can be evicted without an extra map
// scan.
type readNode struct {
	path         string
	entry        ReadEntry
	coverage     []readSourceRange
	emptyVisible bool
	page         *readPageReceipt
	legacyFull   bool
}

type readSourceRange struct{ start, end int }

// A receipt is valid only for the latest pinned read revision. Keep the prior
// confirmed coverage so a negative receipt can revoke this page's contribution
// without discarding bytes that were already visible from earlier pages.
type readPageReceipt struct {
	revision       uint64
	start, end     int
	eligible       bool
	priorCoverage  []readSourceRange
	priorEmptySeen bool
}

const readCoverageMaxRanges = 1024

// NewReadFileState returns a fresh, session-scoped store.
func NewReadFileState() *ReadFileState {
	return &ReadFileState{
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

// Record updates the entry for path with a *full-file* snapshot.
// Caller is asserting that content is the entire on-disk contents —
// IsPartialView will be false. Use RecordPartial when the read was
// truncated by offset/limit.
//
// Evicts the least-recently-recorded entry if the LRU exceeds
// ReadStateMaxEntries.
func (s *ReadFileState) Record(path string, mtime time.Time, content []byte) {
	s.recordFixed(readStatePathKey(path), mtime, content)
}

func (s *ReadFileState) recordFixed(key string, mtime time.Time, content []byte) uint64 {
	return s.recordEntry(filepath.Clean(key), ReadEntry{
		MTime:         mtime,
		Hash:          hashBytes(content),
		ReadAt:        time.Now(),
		Size:          int64(len(content)),
		Offset:        1,
		Limit:         0,
		IsPartialView: false,
	})
}

// RecordPartial records a truncated snapshot — the model only saw
// lines [offset, offset+limit). Write will refuse on this entry until a full
// Read replaces it; targeted Edit may proceed only while the full-file hash
// below still matches.
//
// content here is still the FULL file bytes (used for the hash
// staleness check); offset/limit describe what the LLM was shown.
func (s *ReadFileState) RecordPartial(path string, mtime time.Time, content []byte, offset, limit int) {
	s.recordPartialFixed(readStatePathKey(path), mtime, content, offset, limit)
}

func (s *ReadFileState) recordPartialFixed(key string, mtime time.Time, content []byte, offset, limit int) uint64 {
	return s.recordEntry(filepath.Clean(key), ReadEntry{
		MTime:         mtime,
		Hash:          hashBytes(content),
		ReadAt:        time.Now(),
		Size:          int64(len(content)),
		Offset:        offset,
		Limit:         limit,
		IsPartialView: true,
	})
}

func (s *ReadFileState) recordReadPageFixed(key string, mtime time.Time, content []byte, start, end int, eligible, pending bool) uint64 {
	if s == nil {
		return 0
	}
	entry := ReadEntry{MTime: mtime, Hash: hashBytes(content), ReadAt: time.Now(), Size: int64(len(content)), Offset: 1, IsPartialView: true}
	key = filepath.Clean(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRevision++
	entry.Revision = s.nextRevision
	var node *readNode
	if el, ok := s.entries[key]; ok {
		node = el.Value.(*readNode)
		if node.entry.Hash != entry.Hash || node.entry.Size != entry.Size {
			node.coverage = nil
			node.emptyVisible = false
		}
		s.order.MoveToFront(el)
	} else {
		node = &readNode{path: key}
		s.entries[key] = s.order.PushFront(node)
	}
	node.entry = entry
	node.legacyFull = false
	node.page = &readPageReceipt{
		revision: entry.Revision, start: start, end: end,
		eligible:      eligible && start >= 0 && start <= end && end <= len(content),
		priorCoverage: append([]readSourceRange(nil), node.coverage...), priorEmptySeen: node.emptyVisible,
	}
	if !pending {
		confirmReadPageLocked(node, true)
	} else {
		// Even with earlier confirmed coverage, a new dispatched Read stays
		// pending until its own result projection supplies a receipt.
		node.entry.IsPartialView = true
	}
	s.evictLocked()
	return entry.Revision
}

func (s *ReadFileState) recordEntry(path string, entry ReadEntry) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRevision++
	entry.Revision = s.nextRevision
	if el, ok := s.entries[path]; ok {
		el.Value = newLegacyReadNode(path, entry)
		s.order.MoveToFront(el)
		return entry.Revision
	}
	el := s.order.PushFront(newLegacyReadNode(path, entry))
	s.entries[path] = el
	s.evictLocked()
	return entry.Revision
}

func newLegacyReadNode(path string, entry ReadEntry) *readNode {
	node := &readNode{path: path, entry: entry, legacyFull: !entry.IsPartialView}
	if node.legacyFull {
		if entry.Size == 0 {
			node.emptyVisible = true
		} else {
			node.coverage = []readSourceRange{{0, int(entry.Size)}}
		}
	}
	return node
}

func (s *ReadFileState) evictLocked() {
	for s.order.Len() > ReadStateMaxEntries {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.order.Remove(oldest)
		delete(s.entries, oldest.Value.(*readNode).path)
	}
}

func (s *ReadFileState) confirmVisible(key, hash string, revision uint64, visible bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.entries[filepath.Clean(key)]; ok {
		node := el.Value.(*readNode)
		entry := &node.entry
		if entry.Hash == hash && entry.Revision == revision {
			if node.page != nil {
				confirmReadPageLocked(node, visible)
			} else if node.legacyFull {
				// Compatibility for full snapshots recorded by trusted callers.
				// Generic RecordPartial has no source-range evidence to confirm.
				entry.IsPartialView = !visible
				if !visible {
					node.coverage, node.emptyVisible = nil, false
				} else if entry.Size == 0 {
					node.emptyVisible = true
				} else {
					node.coverage = []readSourceRange{{0, int(entry.Size)}}
				}
			}
		}
	}
}

func confirmReadPageLocked(node *readNode, visible bool) {
	page := node.page
	if page == nil || page.revision != node.entry.Revision {
		return
	}
	node.coverage = append([]readSourceRange(nil), page.priorCoverage...)
	node.emptyVisible = page.priorEmptySeen
	if visible && page.eligible {
		if node.entry.Size == 0 {
			node.emptyVisible = true
		} else if page.start < page.end {
			node.coverage = mergeReadCoverage(node.coverage, readSourceRange{page.start, page.end})
		}
	}
	node.entry.IsPartialView = !readCoverageComplete(node)
}

func readCoverageComplete(node *readNode) bool {
	if node.entry.Size == 0 {
		return node.emptyVisible
	}
	return len(node.coverage) == 1 && node.coverage[0].start == 0 && int64(node.coverage[0].end) == node.entry.Size
}

func mergeReadCoverage(ranges []readSourceRange, next readSourceRange) []readSourceRange {
	ranges = append(ranges, next)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := ranges[:0]
	for _, current := range ranges {
		if len(merged) == 0 || merged[len(merged)-1].end < current.start {
			merged = append(merged, current)
		} else if current.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = current.end
		}
	}
	// Sequential paging coalesces to one range. Bound adversarially sparse
	// one-byte reads without ever manufacturing complete coverage.
	if len(merged) > readCoverageMaxRanges {
		return nil
	}
	return merged
}

// Get returns the recorded entry for path, if any. Does NOT bump LRU
// position — Record is the natural "I just touched this" signal, and
// stale-check Gets shouldn't keep dead entries warm.
func (s *ReadFileState) Get(path string) (ReadEntry, bool) {
	return s.getFixed(readStatePathKey(path))
}

func (s *ReadFileState) getFixed(key string) (ReadEntry, bool) {
	if s == nil {
		return ReadEntry{}, false
	}
	key = filepath.Clean(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.entries[key]
	if !ok {
		return ReadEntry{}, false
	}
	return el.Value.(*readNode).entry, true
}

func (s *ReadFileState) deleteFixed(key string) {
	if s == nil {
		return
	}
	key = filepath.Clean(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.entries[key]; ok {
		s.order.Remove(el)
		delete(s.entries, key)
	}
}

// Reset clears all entries. Called at session boundary so the next
// session starts with a clean slate (new conversation, new files).
func (s *ReadFileState) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]*list.Element)
	s.order = list.New()
}

// Len returns the number of tracked entries. Test helper / diagnostics
// — not used in production code paths.
func (s *ReadFileState) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// hashBytes returns the same encoding Record uses, so callers can
// compare against ReadEntry.Hash without re-implementing.
func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
