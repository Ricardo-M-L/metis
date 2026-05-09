package builtin

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// ReadFileState records, per session, when a file was last seen by a
// reading tool (Read, NotebookEdit's read pass) so subsequent writing
// tools (Edit, Write, NotebookEdit) can detect that:
//
//  1. the on-disk contents drifted between the model's mental snapshot
//     and the write (mtime / hash check), or
//  2. the snapshot itself was partial (offset/limit truncated the
//     view). Editing under (2) is unsafe — the model may rewrite a
//     region of the file it never saw, silently losing those bytes.
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
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front = most recently recorded
}

// ReadStateMaxEntries caps the number of files tracked at once.
// 100 matches claude-code fileTracker; longer histories would inflate
// memory without obvious benefit since edits cluster around a small
// working set.
const ReadStateMaxEntries = 100

// ReadEntry is the snapshot a Read tool captures.
type ReadEntry struct {
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
	// IsPartialView fires Edit/Write's "re-Read first" guard.
	Offset        int
	Limit         int
	IsPartialView bool
}

// readNode is the LRU list payload — pairs a path with its entry so
// the back-pointer in the list can be evicted without an extra map
// scan.
type readNode struct {
	path  string
	entry ReadEntry
}

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
	s.recordEntry(path, ReadEntry{
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
// lines [offset, offset+limit). Edit/Write will refuse on this entry
// until a full Read replaces it; otherwise a write could overwrite
// regions of the file the model never observed.
//
// content here is still the FULL file bytes (used for the hash
// staleness check); offset/limit describe what the LLM was shown.
func (s *ReadFileState) RecordPartial(path string, mtime time.Time, content []byte, offset, limit int) {
	s.recordEntry(path, ReadEntry{
		MTime:         mtime,
		Hash:          hashBytes(content),
		ReadAt:        time.Now(),
		Size:          int64(len(content)),
		Offset:        offset,
		Limit:         limit,
		IsPartialView: true,
	})
}

func (s *ReadFileState) recordEntry(path string, entry ReadEntry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.entries[path]; ok {
		el.Value.(*readNode).entry = entry
		s.order.MoveToFront(el)
		return
	}
	el := s.order.PushFront(&readNode{path: path, entry: entry})
	s.entries[path] = el
	for s.order.Len() > ReadStateMaxEntries {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.order.Remove(oldest)
		delete(s.entries, oldest.Value.(*readNode).path)
	}
}

// Get returns the recorded entry for path, if any. Does NOT bump LRU
// position — Record is the natural "I just touched this" signal, and
// stale-check Gets shouldn't keep dead entries warm.
func (s *ReadFileState) Get(path string) (ReadEntry, bool) {
	if s == nil {
		return ReadEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.entries[path]
	if !ok {
		return ReadEntry{}, false
	}
	return el.Value.(*readNode).entry, true
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
