// Package session trace: immutable per-event trace log with a
// lightweight full-text index, mirroring harness's session-event
// system. One JSONL file per session under ~/.metis/traces/.
//
// The trace is fed by agent.SetTraceHook (see internal/agent/trace.go)
// at runtime assembly: every Event from the main loop AND sub-agent
// loops lands here with its ParentID, so the stored events form a
// spawn-tree (turn -> tool call -> result, nested sub-agents).
//
// Sessions load lazily: NewTraceStore only creates the directory, and
// a session's JSONL is read into memory on first touch (Append, Events,
// Search, NextTurn). This keeps process boot O(1) regardless of how
// many trace files have accumulated on disk.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// TraceEvent is one persisted agent-loop event. Field set is a stable
// subset of agent.Event - everything an observer (the model, the web
// UI, an audit tool) needs without dragging channels or internals in.
type TraceEvent struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	Turn       int       `json:"turn"`
	Sequence   int64     `json:"sequence"`
	Kind       string    `json:"kind"` // text | tool_start | tool_result | turn_end | tokens | error | subagent_start | ...
	TS         time.Time `json:"ts"`
	ParentID   string    `json:"parent_id,omitempty"` // tool_use_id of the event this nests under
	ToolName   string    `json:"tool_name,omitempty"`
	ToolUseID  string    `json:"tool_use_id,omitempty"`
	Text       string    `json:"text,omitempty"`
	IsError    bool      `json:"is_error,omitempty"`
	ElapsedMs  int64     `json:"elapsed_ms,omitempty"`
	SubAgentOf string    `json:"subagent_of,omitempty"` // parent's tool_use_id when forwarded from a sub-agent
}

// traceWriter pairs a session's buffered writer with its underlying
// file so Close can release the descriptor.
type traceWriter struct {
	w *bufio.Writer
	f *os.File
}

// TraceStore appends TraceEvents to JSONL and serves searches over an
// in-memory inverted index. Safe for concurrent use.
type TraceStore struct {
	dir string

	mu      sync.RWMutex
	writers map[string]*traceWriter        // sessionID -> open append writer
	loaded  map[string]bool                // sessionID -> already read from disk
	seq     map[string]int64               // sessionID -> last sequence
	index   map[string]map[string][]string // token -> sessionID -> event IDs
	events  map[string][]*TraceEvent       // sessionID -> ordered events
	ids     map[string]struct{}            // event ID -> exists (ingest dedup)
	turned  map[string]int                 // sessionID -> current turn index
}

// NewTraceStore opens (lazily creating) the trace directory. Existing
// JSONL files are not read here; each session loads on first use.
func NewTraceStore(dir string) (*TraceStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &TraceStore{
		dir:     dir,
		writers: make(map[string]*traceWriter),
		loaded:  make(map[string]bool),
		seq:     make(map[string]int64),
		index:   make(map[string]map[string][]string),
		events:  make(map[string][]*TraceEvent),
		ids:     make(map[string]struct{}),
		turned:  make(map[string]int),
	}, nil
}

// validTraceSessionID rejects IDs that could escape the trace dir.
func validTraceSessionID(sid string) bool {
	if sid == "" || sid == "." || sid == ".." {
		return false
	}
	if strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		return false
	}
	if strings.IndexFunc(sid, unicode.IsControl) >= 0 {
		return false
	}
	return true
}

// loadFile reads one session's JSONL. Individual corrupt lines are
// skipped so a single bad write cannot erase the session.
func (s *TraceStore) loadFile(path string) []*TraceEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil // no file yet - nothing recorded for this session
	}
	defer f.Close()
	var out []*TraceEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev TraceEvent
		if json.Unmarshal(line, &ev) == nil && ev.ID != "" {
			cp := ev
			out = append(out, &cp)
		}
	}
	return out
}

// ensureLoadedLocked reads a session's file into memory once. Caller
// holds mu (write) or has otherwise exclusive access. Marked loaded
// even on read errors so an unreadable file is not re-read on every
// call; the in-memory view then simply starts empty and Append will
// still write (and read back) through the same file.
func (s *TraceStore) ensureLoadedLocked(sid string) {
	if sid == "" || s.loaded[sid] {
		return
	}
	s.loaded[sid] = true
	for _, ev := range s.loadFile(filepath.Join(s.dir, sid+".jsonl")) {
		s.ingest(sid, ev)
	}
}

func (s *TraceStore) ensureLoaded(sid string) {
	s.mu.RLock()
	done := s.loaded[sid]
	s.mu.RUnlock()
	if done {
		return
	}
	s.mu.Lock()
	s.ensureLoadedLocked(sid)
	s.mu.Unlock()
}

// discoverSessions lists session IDs present on disk (for cross-session
// search). The trace dir may legitimately not exist in tests.
func (s *TraceStore) discoverSessions() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		sid := strings.TrimSuffix(e.Name(), ".jsonl")
		if validTraceSessionID(sid) {
			out = append(out, sid)
		}
	}
	return out
}

// ingest indexes an event in memory. Idempotent: an already-ingested event
// is skipped so a reload does not double-count the index.
func (s *TraceStore) ingest(sid string, ev *TraceEvent) {
	if ev.Sequence > s.seq[sid] {
		s.seq[sid] = ev.Sequence
	}
	if ev.Turn > s.turned[sid] {
		s.turned[sid] = ev.Turn
	}
	// O(1) dedup by event ID (a linear scan made reload O(n^2)).
	if _, ok := s.ids[ev.ID]; ok {
		return
	}
	s.ids[ev.ID] = struct{}{}
	seen := make(map[string]struct{}, 8)
	for _, tok := range tokenize(ev.Text + " " + ev.ToolName + " " + ev.Kind) {
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		bySess := s.index[tok]
		if bySess == nil {
			bySess = make(map[string][]string)
			s.index[tok] = bySess
		}
		bySess[sid] = append(bySess[sid], ev.ID)
	}
	s.events[sid] = append(s.events[sid], ev)
}

// Append adds a trace event to the session's JSONL and index. Cheap:
// writes are buffered, flushed on Sync / Close / every 200 events.
func (s *TraceStore) Append(ev *TraceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(ev)
}

func (s *TraceStore) appendLocked(ev *TraceEvent) error {
	if !validTraceSessionID(ev.SessionID) {
		return fmt.Errorf("trace: invalid session_id %q", ev.SessionID)
	}
	s.ensureLoadedLocked(ev.SessionID)
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	sid := ev.SessionID
	if ev.Turn == 0 {
		ev.Turn = s.turned[sid]
	}
	s.seq[sid]++
	ev.Sequence = s.seq[sid]

	// Write to disk first. If this fails (disk full, permission, I/O error),
	// we do NOT ingest into memory - the seq counter stays consistent with
	// disk, and a reload will not double-count or lose sequence alignment.
	w := s.writers[sid]
	if w == nil {
		f, err := os.OpenFile(filepath.Join(s.dir, sid+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		w = &traceWriter{w: bufio.NewWriterSize(f, 64*1024), f: f}
		s.writers[sid] = w
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(raw); err != nil {
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return err
	}
	if s.seq[sid]%200 == 0 {
		if err := w.w.Flush(); err != nil {
			return err
		}
	}
	// Only ingest into memory after the disk write succeeded.
	s.ingest(sid, ev)
	return nil
}

// NextTurn starts a new turn for the session (called once per user
// prompt). Returns the new turn index.
func (s *TraceStore) NextTurn(sid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(sid)
	s.turned[sid]++
	return s.turned[sid]
}

// Events returns all trace events for a session, in order.
func (s *TraceStore) Events(sid string) []TraceEvent {
	if !validTraceSessionID(sid) {
		return nil // defensive: model-supplied ids must not reach the FS
	}
	s.ensureLoaded(sid)
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.events[sid]
	out := make([]TraceEvent, 0, len(src))
	for _, ev := range src {
		out = append(out, *ev)
	}
	return out
}

// Trace returns the events as a child-ordered tree suitable for
// nested rendering. Root events (ParentID == "") come first, each
// followed by its descendants (indented one level). The slice holds
// (event, depth) pairs.
func (s *TraceStore) Trace(sid string) []TracedNode {
	evs := s.Events(sid)
	byParent := map[string][]TraceEvent{}     // ParentID -> children
	bySubAgentOf := map[string][]TraceEvent{} // SubAgentOf -> children
	var roots []TraceEvent
	for _, ev := range evs {
		if ev.ParentID != "" {
			byParent[ev.ParentID] = append(byParent[ev.ParentID], ev)
		} else if ev.SubAgentOf != "" {
			bySubAgentOf[ev.SubAgentOf] = append(bySubAgentOf[ev.SubAgentOf], ev)
		} else {
			roots = append(roots, ev)
		}
	}
	var nodes []TracedNode
	var walk func(ev TraceEvent, depth int)
	walk = func(ev TraceEvent, depth int) {
		nodes = append(nodes, TracedNode{Event: ev, Depth: depth})
		// Only a tool_start owns children. Streaming tool_args and the
		// eventual tool_result share its ToolUseID, but neither is a parent;
		// letting an earlier args delta consume the key detaches the result
		// from the actual call and can create a self-cycle on tool_result.
		parentKey := ""
		if ev.Kind == "tool_start" || ev.Kind == "subagent_start" {
			parentKey = ev.ToolUseID
		}
		kids := byParent[parentKey]
		if parentKey != "" {
			delete(byParent, parentKey)
		}
		for _, child := range kids {
			walk(child, depth+1)
		}
		kids2 := bySubAgentOf[parentKey]
		if parentKey != "" {
			delete(bySubAgentOf, parentKey)
		}
		for _, child := range kids2 {
			walk(child, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return nodes
}

// TracedNode couples an event with its nesting depth in the trace tree.
type TracedNode struct {
	Event TraceEvent `json:"event"`
	Depth int        `json:"depth"`
}

// Search runs a full-text query over trace events. Query terms are
// AND-ed; every term must appear (case-insensitive, tokenized) in an
// event's text/tool/kind fields. limit caps results (0 = no limit).
// sessionID optionally narrows the search to one session.
func (s *TraceStore) Search(query, sessionID string, limit int) ([]TraceEvent, error) {
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil, fmt.Errorf("trace: empty query")
	}
	if sessionID != "" {
		if !validTraceSessionID(sessionID) {
			return nil, fmt.Errorf("trace: invalid session_id %q", sessionID)
		}
		s.ensureLoaded(sessionID)
	} else {
		// Cross-session search must see every recorded session.
		for _, sid := range s.discoverSessions() {
			s.ensureLoaded(sid)
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Candidate set = intersection of term -> event IDs across (optionally
	// session-filtered) sessions.
	var candidates map[string]struct{}
	for _, tok := range terms {
		matched := make(map[string]struct{})
		bySess := s.index[tok]
		if len(bySess) == 0 {
			return nil, nil // a term matches nothing -> AND query yields nothing
		}
		for sid, idList := range bySess {
			if sessionID != "" && sid != sessionID {
				continue
			}
			for _, id := range idList {
				matched[id] = struct{}{}
			}
		}
		if candidates == nil {
			candidates = matched
		} else {
			for id := range candidates {
				if _, ok := matched[id]; !ok {
					delete(candidates, id)
				}
			}
		}
		if len(candidates) == 0 {
			return nil, nil
		}
	}

	// Resolve candidate IDs -> events, preserving per-session order.
	indexed := make(map[string]*TraceEvent, len(candidates))
	var out []TraceEvent
	for _, evs := range s.events {
		for _, ev := range evs {
			if _, ok := candidates[ev.ID]; ok {
				if indexed[ev.ID] == nil { // first occurrence wins (dedupe)
					indexed[ev.ID] = ev
					out = append(out, *ev)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if sessionID != "" { // single-session: skip per-session-ID compare
			return out[i].Sequence < out[j].Sequence
		}
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].Sequence < out[j].Sequence
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Sync flushes buffered writes for all sessions (files stay open).
func (s *TraceStore) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, w := range s.writers {
		if err := w.w.Flush(); err != nil {
			return fmt.Errorf("trace: flush %s: %w", sid, err)
		}
	}
	return nil
}

// Delete removes one session's persisted trace and all of its in-memory
// state. Writers are flushed and closed while the store lock is held so no
// concurrent Append can write through an unlinked file descriptor. A missing
// trace is treated as already deleted.
func (s *TraceStore) Delete(sessionID string) error {
	if !validTraceSessionID(sessionID) {
		return fmt.Errorf("trace: invalid session_id %q", sessionID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var deleteErr error
	if w := s.writers[sessionID]; w != nil {
		if err := w.w.Flush(); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("trace: flush %s: %w", sessionID, err))
		}
		if err := w.f.Close(); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("trace: close %s: %w", sessionID, err))
		}
		delete(s.writers, sessionID)
	}

	for _, ev := range s.events[sessionID] {
		delete(s.ids, ev.ID)
	}
	for token, bySession := range s.index {
		delete(bySession, sessionID)
		if len(bySession) == 0 {
			delete(s.index, token)
		}
	}
	delete(s.loaded, sessionID)
	delete(s.seq, sessionID)
	delete(s.events, sessionID)
	delete(s.turned, sessionID)

	path := filepath.Join(s.dir, sessionID+".jsonl")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		deleteErr = errors.Join(deleteErr, fmt.Errorf("trace: remove %s: %w", sessionID, err))
	}
	return deleteErr
}

// Close flushes and closes all writers and their files.
func (s *TraceStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for sid, w := range s.writers {
		if err := w.w.Flush(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		if err := w.f.Close(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("trace: close %s: %w", sid, err)
			}
		}
	}
	s.writers = map[string]*traceWriter{}
	return firstErr
}

// isCJKRune reports whether r belongs to a script without spaces
// between words (CJK ideographs, kana, hangul).
func isCJKRune(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0x3400 && r <= 0x4DBF: // Extension A
		return true
	case r >= 0xF900 && r <= 0xFAFF: // Compatibility Ideographs
		return true
	case r >= 0x3040 && r <= 0x30FF: // Hiragana + Katakana
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul syllables
		return true
	}
	return false
}

// tokenize splits text into lowercase search tokens. ASCII words are
// kept whole. CJK runs have no word boundaries, so pure word-splitting
// would drop them entirely and make Chinese text unsearchable; those
// runs therefore also emit unigrams and bigrams (index and query use
// the same tokenizer, so a contiguous query substring matches).
func tokenize(text string) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		for _, r := range cjk {
			out = append(out, string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			out = append(out, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			flushCJK()
			word = append(word, r)
		case isCJKRune(r):
			flushWord()
			cjk = append(cjk, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

// TruncateTrace truncates text to at most n runes (not bytes), appending
// an ellipsis suffix when the original exceeds n. Rune-safe for CJK.
func TruncateTrace(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...(truncated)"
}
