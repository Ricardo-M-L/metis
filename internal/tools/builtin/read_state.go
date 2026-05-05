package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// ReadFileState records, per session, when a file was last seen by a
// reading tool (Read, NotebookEdit's read pass) so subsequent writing
// tools (Edit, Write, NotebookEdit) can detect that the on-disk
// contents drifted between the model's mental snapshot and the write.
//
// Mirrors claude-code's readFileState in tools/FileEditTool/utils.ts.
// The check sits between the staleness Read (mtime ⇒ unchanged) and
// the actual write — see Edit.Execute and Write.Execute below.
//
// Concurrency: Edit / Write are ConcurrencyExclusive so they serialize
// after the parallel Read / Grep batch in dispatch.go. The Mutex
// here only guards multi-session reuse of the same store; intra-batch
// the dispatch tier ordering already prevents read-then-edit races.
type ReadFileState struct {
	mu      sync.RWMutex
	entries map[string]ReadEntry
}

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
}

// NewReadFileState returns a fresh, session-scoped store.
func NewReadFileState() *ReadFileState {
	return &ReadFileState{entries: make(map[string]ReadEntry)}
}

// Record updates the entry for path.
func (s *ReadFileState) Record(path string, mtime time.Time, content []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := sha256.Sum256(content)
	s.entries[path] = ReadEntry{
		MTime:  mtime,
		Hash:   hex.EncodeToString(h[:]),
		ReadAt: time.Now(),
	}
}

// Get returns the recorded entry for path, if any.
func (s *ReadFileState) Get(path string) (ReadEntry, bool) {
	if s == nil {
		return ReadEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[path]
	return e, ok
}

// Reset clears all entries. Called at session boundary so the next
// session starts with a clean slate (new conversation, new files).
func (s *ReadFileState) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]ReadEntry)
}

// hashBytes returns the same encoding Record uses, so callers can
// compare against ReadEntry.Hash without re-implementing.
func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
