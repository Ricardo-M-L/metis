package agent

// touched_files.go — session-scoped file activity report.
//
// The data has always been in Loop.Messages (Read / Edit / Write
// tool_use blocks carry the path on their input maps). Before this
// file, the only consumer was extractRecentToolInputPaths in
// post_compact_attachments.go — a private compact-internal helper.
// This file lifts the same scan into a public API so /diff and
// future /files can show "what did this session touch?" without
// each surface re-implementing the walk.
//
// Why this matters: `git diff` is not enough. It only shows
// uncommitted edits AND mixes the user's prior edits with the
// agent's. claude-code solves this with full file-history
// snapshots (utils/fileHistory.ts); crush ships a sqlite
// filetracker.Service that records last-read timestamps per
// (session, path). metis takes the lightweight middle path:
// derive the list from the message log (no extra storage, no
// extra writes, zero cost), pair it with `git diff --stat` for
// the actual content delta. Good enough for "what was the agent
// working on" without adding a new persistence layer.

import (
	"sort"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TouchedFile is the per-path summary returned by TouchedFiles. Counts
// reflect tool-call frequency — a model that Edited the same file 3
// times in this session reports Edits=3. LastTurnIdx is the highest
// (newest) message index that mentioned this path, used as the
// secondary sort key so the most-recent activity bubbles up.
type TouchedFile struct {
	Path        string
	Reads       int
	Edits       int
	Writes      int
	LastTurnIdx int // highest message index referencing this path
}

// IsModified reports whether the path was actually mutated by the
// agent (Edit or Write). Pure reads don't count as modifications.
func (t TouchedFile) IsModified() bool {
	return t.Edits > 0 || t.Writes > 0
}

// TouchedFiles walks messages oldest-first and returns one
// TouchedFile per distinct path mentioned by Read / Edit / Write /
// NotebookEdit tool_use blocks. Output is sorted: modified files
// first (Edit / Write count > 0), then by LastTurnIdx descending so
// the most recent activity is at the top.
//
// Pure read-only tools (Glob, Grep, LS, Bash) are intentionally NOT
// counted — their inputs are patterns / commands, not concrete file
// paths, and surfacing them as "touched" would drown the signal in
// search noise. If a future need arises (e.g. show "directories
// scanned"), add it as a separate API rather than overloading this
// one.
//
// Returns nil when messages contains no recognized tool_use entries
// with path inputs. Safe to call on a freshly-built Loop with empty
// history.
func TouchedFiles(messages []llm.Message) []TouchedFile {
	if len(messages) == 0 {
		return nil
	}
	idx := make(map[string]*TouchedFile)
	for i, m := range messages {
		for _, b := range m.Content {
			if b.Type != "tool_use" {
				continue
			}
			path := stringFieldAny(b.ToolInput, "path", "file_path")
			if path == "" {
				continue
			}
			entry, ok := idx[path]
			if !ok {
				entry = &TouchedFile{Path: path}
				idx[path] = entry
			}
			entry.LastTurnIdx = i
			switch b.ToolName {
			case "Read":
				entry.Reads++
			case "Edit":
				entry.Edits++
			case "Write":
				entry.Writes++
			case "NotebookEdit":
				// Treat notebook cells as edits for the user-facing
				// view; the path identifies the .ipynb file.
				entry.Edits++
			}
		}
	}
	out := make([]TouchedFile, 0, len(idx))
	for _, e := range idx {
		// Drop entries where no recognized tool name matched (the
		// path was extracted from an unrelated input shape). Counts
		// stay at zero in that case.
		if e.Reads+e.Edits+e.Writes == 0 {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		// Modified first.
		if out[i].IsModified() != out[j].IsModified() {
			return out[i].IsModified()
		}
		// Then most-recent activity.
		return out[i].LastTurnIdx > out[j].LastTurnIdx
	})
	return out
}

// TouchedFiles returns the session's file activity report. Acquires
// the Loop's read lock so the slice is stable; callers should not
// retain references to the underlying message slice (we copy out
// into TouchedFile structs anyway). Safe to call concurrently with
// the agent loop.
func (l *Loop) TouchedFiles() []TouchedFile {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return TouchedFiles(l.Messages)
}
