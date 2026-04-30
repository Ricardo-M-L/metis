package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// HistoryEntry is one row in ~/.metis/history.jsonl.
//
// Mirror of the readline `~/.metis/history` file but structured: each
// line carries the timestamp + session id + raw input so a third-party
// dashboard or `metis history grep` can answer "what did I ask in
// October?" without trawling 200+ session JSONLs.
//
// We append on every user submit. The plain-text readline file stays
// authoritative for ↑↓ history navigation in the editor — JSONL is
// strictly an additive observability layer.
type HistoryEntry struct {
	Timestamp time.Time `json:"ts"`
	SessionID string    `json:"session"`
	Input     string    `json:"input"`
	// Source distinguishes the surface: "tui" (bubbletea) vs "repl"
	// (readline fallback). Useful when correlating history with bug
	// reports.
	Source string `json:"source"`
}

// HistoryJSONLPath returns the canonical path of history.jsonl under the
// metis home.
func HistoryJSONLPath() string {
	return filepath.Join(config.Home(), "history.jsonl")
}

// historyMu serializes appends so concurrent TUI / REPL writers can't
// interleave half-written JSON lines. Process-wide because both
// surfaces target the same file.
var historyMu sync.Mutex

// AppendHistory writes one HistoryEntry as a single JSON line. Errors
// are non-fatal — we never want a disk hiccup to block the chat surface
// — so callers fire-and-forget.
//
// Empty Input is silently dropped: the editor often emits empty lines
// (just hitting Enter) and they pollute history with no value.
func AppendHistory(entry HistoryEntry) error {
	if entry.Input == "" {
		return nil
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	dir := config.Home()
	if dir == "" {
		return errors.New("history: no $METIS_HOME / $HOME")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("history: mkdir: %w", err)
	}

	historyMu.Lock()
	defer historyMu.Unlock()

	f, err := os.OpenFile(HistoryJSONLPath(),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("history: open: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(entry)
}

// LoadRecentHistory returns the most recent N HistoryEntry rows in
// reverse chronological order (newest first). Used by the Ctrl+R
// fuzzy-search overlay to populate its candidate list — it doesn't
// need the full file, just the last few hundred prompts.
//
// Missing file is not an error (returns empty slice) — first-run users
// have no history and the overlay should still open.
func LoadRecentHistory(limit int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	path := HistoryJSONLPath()
	historyMu.Lock()
	defer historyMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Read the whole file linearly. JSONL parsing is cheap; for files
	// 50k+ lines we'd want a tail-read, but realistic metis usage tops
	// out around 5k lines (one per prompt) which is microseconds to
	// scan. Keep the simple path.
	var all []HistoryEntry
	dec := json.NewDecoder(f)
	for dec.More() {
		var e HistoryEntry
		if err := dec.Decode(&e); err != nil {
			// Skip malformed lines instead of bailing — partial writes
			// after a crash shouldn't break Ctrl+R.
			continue
		}
		if e.Input == "" {
			continue
		}
		all = append(all, e)
	}
	// Newest first.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
