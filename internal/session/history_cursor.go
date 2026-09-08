package session

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// HistoryCursor records which prefix of an agent loop's in-memory history has
// already been appended to a session JSONL file. The last persisted message is
// retained as an anchor so the cursor can relocate after compaction, undo, or
// any other operation that replaces the history slice with a shorter one.
//
// The fields are deliberately private: callers can only advance the cursor by
// successfully appending messages (AppendHistoryTail), or explicitly mark an
// already-persisted snapshot at a known session boundary (Mark).
type HistoryCursor struct {
	count    int
	last     *llm.Message
	lastJSON []byte
}

// NewHistoryCursor returns a cursor positioned after history. Use it when the
// supplied history was loaded from the same session file and is therefore
// already durable.
func NewHistoryCursor(history []llm.Message) HistoryCursor {
	var c HistoryCursor
	c.Mark(history)
	return c
}

// Mark positions the cursor after history without writing it. This is used
// after a session load/branch, and after a caller has intentionally persisted a
// display-safe variant of the last message (for example cmd/metis run stores
// the raw prompt while the loop holds an LLM-only prompt with injected hints).
func (c *HistoryCursor) Mark(history []llm.Message) {
	if c == nil {
		return
	}
	c.count = len(history)
	c.last = nil
	c.lastJSON = nil
	if len(history) > 0 {
		// An ordinary struct copy retains Content slices and nested maps. Those
		// are mutated by compaction and provider-state cleanup, which would also
		// mutate the supposed durable anchor. Snapshot the persisted form and
		// compare that form below: JSON round trips can normalize numeric types,
		// while in-memory-only fields intentionally never reached the ledger.
		encoded, err := json.Marshal(history[len(history)-1])
		if err != nil {
			// Mark has no error return. An unencodable boundary cannot be trusted;
			// a later append will try a replacement and surface serialization errors.
			return
		}
		var last llm.Message
		if err := json.Unmarshal(encoded, &last); err != nil {
			return
		}
		c.last = &last
		c.lastJSON = encoded
	}
}

// AppendHistoryTail appends every not-yet-persisted message in history, in
// order. The cursor advances after each successful line, so a partial write can
// be retried without duplicating the prefix that reached disk.
//
// A simple integer alone becomes stale when compaction or undo shortens or
// rewrites loop history. When the durable anchor is no longer exactly at the
// cursor boundary, appending a suffix cannot invalidate the old JSONL prefix;
// write a full history_replace snapshot instead. The reverse anchor search
// deliberately finds the last duplicate anchor: this prevents a repeated
// message later in the new history from making us skip intervening unwritten
// content. Anchors are compared in their persisted JSON form so in-memory-only
// metadata and numeric normalization do not cause spurious full replacements.
func (s *Store) AppendHistoryTail(id string, history []llm.Message, cursor *HistoryCursor) error {
	if s == nil || id == "" {
		return nil
	}
	if cursor == nil {
		return fmt.Errorf("append history tail: nil cursor")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendHistoryTailLocked(id, history, cursor)
}

// appendHistoryTailLocked is AppendHistoryTail with the store lock already
// held. CheckpointCompaction uses it so raw-tail append, fsync, and replacement
// are one ordered transaction with respect to other writers on this Store.
func (s *Store) appendHistoryTailLocked(id string, history []llm.Message, cursor *HistoryCursor) error {
	start := cursor.count
	lastAnchor := -1
	if cursor.last != nil {
		for i := len(history) - 1; i >= 0; i-- {
			encoded, err := json.Marshal(history[i])
			if err == nil && bytes.Equal(encoded, cursor.lastJSON) {
				lastAnchor = i
				break
			}
		}
	}
	anchorMatches := start == 0 || (start <= len(history) && lastAnchor == start-1)
	if start > len(history) || !anchorMatches {
		return s.replaceHistoryAndMarkLocked(id, history, cursor, false)
	}

	for i := start; i < len(history); i++ {
		message := history[i]
		if err := s.appendEntryLocked(id, Entry{Type: "message", Message: &message}, false); err != nil {
			return err
		}
		cursor.Mark(history[:i+1])
	}
	return nil
}

// CheckpointHistory synchronously appends a completed iteration's unwritten
// history and fsyncs it before the caller starts any further provider work.
// Unpaired tool calls/results are rejected before any write; the caller must
// supply actual completed results, never an in-flight tool-use snapshot.
//
// The existing per-message JSONL format is preserved. A failed append may leave
// a visible successful prefix, and cursor tracks that prefix for a safe retry.
// This is not a multi-message atomic transaction: the caller must stop on error
// and retry persistence or use the existing resume repair before provider work.
// A failed fsync also remains retryable, including when every line was already
// appended: every successful call must cross the fsync barrier again.
func (s *Store) CheckpointHistory(id string, history []llm.Message, cursor *HistoryCursor) error {
	if cursor == nil {
		return fmt.Errorf("checkpoint history: nil cursor")
	}
	if s == nil || id == "" {
		return nil
	}
	if err := validateCheckpointToolPairs(history); err != nil {
		return fmt.Errorf("checkpoint history: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendHistoryTailLocked(id, history, cursor); err != nil {
		return fmt.Errorf("checkpoint history append: %w", err)
	}
	if err := s.syncLocked(id); err != nil {
		return fmt.Errorf("checkpoint history sync: %w", err)
	}
	return nil
}

func validateCheckpointToolPairs(history []llm.Message) error {
	pending := make(map[string]struct{})
	for _, message := range history {
		for _, block := range message.Content {
			switch block.Type {
			case "tool_use":
				if block.ToolUseID == "" {
					return fmt.Errorf("tool_use is missing its id")
				}
				if _, exists := pending[block.ToolUseID]; exists {
					return fmt.Errorf("duplicate pending tool_use %q", block.ToolUseID)
				}
				pending[block.ToolUseID] = struct{}{}
			case "tool_result":
				if _, exists := pending[block.ToolUseID]; !exists {
					return fmt.Errorf("tool_result %q has no pending tool_use", block.ToolUseID)
				}
				delete(pending, block.ToolUseID)
			}
		}
	}
	if len(pending) != 0 {
		return fmt.Errorf("history contains %d unfinished tool call(s)", len(pending))
	}
	return nil
}

// replaceHistoryAndMarkLocked appends a replacement and advances cursor only
// after the requested durability boundary succeeds. s.mu must be held.
func (s *Store) replaceHistoryAndMarkLocked(id string, history []llm.Message, cursor *HistoryCursor, durable bool) error {
	if err := s.appendEntryLocked(id, Entry{Type: "history_replace", Messages: history}, durable); err != nil {
		return err
	}
	cursor.Mark(history)
	return nil
}

// CheckpointCompaction durably commits a context replacement without losing
// the raw messages that triggered it. It first appends every message in before
// that is not yet represented by cursor, then appends a history_replace entry
// for after and advances cursor to that exact logical snapshot.
//
// The two append phases intentionally preserve crash semantics:
//   - a crash before history_replace resumes the complete raw before history;
//   - a crash after history_replace resumes the exact compacted after history;
//   - a write error leaves the cursor at the last line that actually reached
//     disk, so CompactNow can roll back memory and a later retry is safe.
//
// The JSONL remains append-only, therefore the pre-compaction messages stay in
// the physical audit ledger even though Load applies the later replacement as
// the current logical conversation.
func (s *Store) CheckpointCompaction(id string, before, after []llm.Message, cursor *HistoryCursor) error {
	if cursor == nil {
		return fmt.Errorf("checkpoint compaction: nil cursor")
	}
	if s == nil || id == "" {
		cursor.Mark(after)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Work on a copy while appending so any successfully visible prefix can be
	// published to the caller as one coherent cursor boundary before fsync.
	rawCursor := *cursor
	if err := s.appendHistoryTailLocked(id, before, &rawCursor); err != nil {
		// Successful prefix entries are already visible in the ledger. Preserve
		// that boundary just as AppendHistoryTail does, so a retry cannot duplicate
		// them even though the final entry failed.
		*cursor = rawCursor
		return fmt.Errorf("checkpoint compaction raw tail: %w", err)
	}
	*cursor = rawCursor
	if err := s.syncLocked(id); err != nil {
		return fmt.Errorf("checkpoint compaction sync raw tail: %w", err)
	}

	// The replacement itself is written and fsynced before its logical cursor
	// becomes visible. A crash before this point resumes the durable raw tail;
	// a crash after it resumes exactly the compacted checkpoint.
	if err := s.replaceHistoryAndMarkLocked(id, after, cursor, true); err != nil {
		return fmt.Errorf("checkpoint compaction replacement: %w", err)
	}
	return nil
}
