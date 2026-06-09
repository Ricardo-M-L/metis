package agent

// checkpoint_hook.go — the loop side of the unified /rewind. Before the
// first file-mutating tool of each user turn, snapPreEdit takes a
// shadow-git snapshot of the working tree (via l.Checkpointer). Rewind
// then restores BOTH the files (checkpoint.Restore) and the conversation
// (UndoLastTurn down to the snapshot's turn) — atomically returning to
// the state before that turn's edits. Mirrors claude-code's Esc-Esc
// rewind, which restores code + transcript together.
//
// Best-effort: any Snap/Restore error is swallowed and leaves the loop
// running normally (a broken shadow repo must never block real work).

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ckptEntry pairs a file snapshot with the conversation point to undo to.
type ckptEntry struct {
	hash           string // shadow-git commit hash to Restore
	restoreToTurns int    // CountTurns() value the conversation should return to
	label          string // human label ("before turn 3")
}

// mutatingTools are the tools whose execution can change the working
// tree, so a pre-edit snapshot is worth taking. Bash is included
// conservatively (it often mutates); the snapshot is cheap relative to
// an LLM turn and only taken once per turn.
var mutatingTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
	"Bash":         true,
}

// snapPreEdit takes a working-tree snapshot before the first mutating
// tool of the current turn. No-op when checkpointing is disabled, the
// tool isn't mutating, or this turn was already snapped.
func (l *Loop) snapPreEdit(toolName string, input map[string]any) {
	if l.Checkpointer == nil || !mutatingTools[toolName] {
		return
	}
	turns := l.CountTurns()
	l.ckptMu.Lock()
	already := l.ckptSnappedAt == turns
	l.ckptMu.Unlock()
	if already {
		return
	}

	// Snap BEFORE marking the slot. A read-only Bash (cat/ls/git status)
	// is in mutatingTools too but produces an empty hash (no diff to
	// commit); a transient Snap error returns "". In both cases we must
	// NOT mark the turn snapped — otherwise the real Edit that follows in
	// the same turn early-returns and the turn loses its only rewind
	// point. Only a real, non-empty snapshot consumes the slot.
	hash, err := l.Checkpointer.Snap(toolName, argsHash(input), fmt.Sprintf("before turn %d", turns))
	if err != nil || hash == "" {
		return
	}
	l.ckptMu.Lock()
	if l.ckptSnappedAt != turns { // re-check: another tool may have won the race
		l.ckptSnappedAt = turns
		l.ckptStack = append(l.ckptStack, ckptEntry{
			hash:           hash,
			restoreToTurns: turns - 1, // rewinding drops this turn's edits + conversation
			label:          fmt.Sprintf("before turn %d", turns),
		})
	}
	l.ckptMu.Unlock()
}

// RewindResult describes what a Rewind did, for the UI to report.
type RewindResult struct {
	Label       string
	TurnsUndone int
}

// Rewind restores the working tree to the most recent pre-edit snapshot
// AND undoes the conversation back to that point. Returns ok=false when
// checkpointing is off or there's nothing to rewind. Mirrors claude-code's
// Esc-Esc: files and transcript move together.
func (l *Loop) Rewind() (RewindResult, bool) {
	if l.Checkpointer == nil {
		return RewindResult{}, false
	}
	l.ckptMu.Lock()
	if len(l.ckptStack) == 0 {
		l.ckptMu.Unlock()
		return RewindResult{}, false
	}
	e := l.ckptStack[len(l.ckptStack)-1]
	l.ckptStack = l.ckptStack[:len(l.ckptStack)-1]
	l.ckptSnappedAt = -1 // let the next turn re-snap
	l.ckptMu.Unlock()

	if err := l.Checkpointer.Restore(e.hash); err != nil {
		return RewindResult{}, false
	}
	undone := 0
	for l.CountTurns() > e.restoreToTurns {
		if !l.UndoLastTurn() {
			break
		}
		undone++
	}
	return RewindResult{Label: e.label, TurnsUndone: undone}, true
}

// HasRewindPoints reports whether there's at least one snapshot to rewind
// to — lets the TUI show /rewind only when it would do something.
func (l *Loop) HasRewindPoints() bool {
	if l.Checkpointer == nil {
		return false
	}
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	return len(l.ckptStack) > 0
}

// argsHash is a short stable digest of a tool's input, stored in the
// checkpoint commit message for debugging (which edit triggered it).
func argsHash(input map[string]any) string {
	b, err := json.Marshal(input)
	if err != nil {
		return "?"
	}
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])[:8]
}
