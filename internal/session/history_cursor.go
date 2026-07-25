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
		return s.ReplaceHistoryAndMark(id, history, cursor)
	}

	for i := start; i < len(history); i++ {
		if err := s.AppendMessage(id, history[i]); err != nil {
			return err
		}
		cursor.count = i + 1
		msg := history[i]
		cursor.last = &msg
	}
	return nil
}
