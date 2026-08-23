package session

import (
	"fmt"
	"reflect"

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
	count int
	last  *llm.Message
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
	if len(history) > 0 {
		last := history[len(history)-1]
		c.last = &last
	}
}

// AppendHistoryTail appends every not-yet-persisted message in history, in
// order. The cursor advances after each successful line, so a partial write can
// be retried without duplicating the prefix that reached disk.
//
// A simple integer alone becomes stale when compaction or undo shortens or
// rewrites loop history. When the durable anchor is no longer exactly at the
// cursor boundary, appending a suffix cannot invalidate the old JSONL prefix;
// write a full history_replace snapshot instead. The reverse DeepEqual search
// deliberately finds the last duplicate anchor: this prevents a repeated
// message later in the new history from making us skip intervening unwritten
// content.
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
			if reflect.DeepEqual(history[i], *cursor.last) {
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
		cursor.count = i + 1
		cursor.last = &message
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
