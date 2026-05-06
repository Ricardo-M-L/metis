package tui

// keybind_history_direct.go — bash/zsh-style direct ↑/↓ prompt history
// (T7). Distinct from Ctrl+R (keybind_history.go) which opens a fuzzy
// search overlay. This file handles the case where the user wants to
// recall the previous prompt without leaving the input box: empty
// input + ↑ → load histAll[0], another ↑ → histAll[1], ↓ steps back.
//
// Inspired by crush internal/ui/model/history.go (handleHistoryUp/Down +
// auto-saved draft). On entering nav mode the user's in-progress text
// is stashed so ↓ past index 0 restores it instead of leaving them with
// an empty input.

import (
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

// directHistoryEligible reports whether ↑/↓ should be intercepted as
// history navigation rather than passed through to the textarea (where
// they'd move the cursor between rows). Two cases qualify:
//
//  1. Input is empty.
//  2. Input was last set by direct-history nav (user hasn't typed since
//     loading a previous prompt) — detected by histDirectIdx ≥ 0.
//
// A multi-line input that the user is actively editing (histDirectIdx
// == -1, value != "") falls through so ↑ moves the cursor as usual.
func (m *Model) directHistoryEligible() bool {
	if m.histDirectIdx >= 0 {
		return true
	}
	return m.input.Value() == ""
}

// loadDirectHistory lazy-loads ~/.metis/history.jsonl into m.histAll on
// first use. Same backing data as the Ctrl+R overlay, so opening that
// overlay later sees identical entries (no double-load).
func (m *Model) loadDirectHistory() {
	if m.histAll != nil {
		return
	}
	entries, _ := runtime.LoadRecentHistory(500)
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Input] {
			continue
		}
		seen[e.Input] = true
		m.histAll = append(m.histAll, e.Input)
	}
}

// directHistoryUp walks one entry older. First press saves the current
// (potentially empty) input as a draft so ↓ past index 0 can restore
// it. Returns true when handled, false to fall through.
func (m *Model) directHistoryUp() bool {
	m.loadDirectHistory()
	if len(m.histAll) == 0 {
		return false
	}
	if m.histDirectIdx < 0 {
		// Save the in-progress text before overwriting it. Empty
		// draft is fine — it'll restore as empty on ↓ past 0.
		m.histDirectDraft = m.input.Value()
		m.histDirectIdx = 0
	} else if m.histDirectIdx < len(m.histAll)-1 {
		m.histDirectIdx++
	} else {
		// Already at the oldest entry — clamp, don't wrap.
		return true
	}
	m.input.SetValue(m.histAll[m.histDirectIdx])
	m.input.CursorEnd()
	return true
}

// directHistoryDown walks one entry newer. Stepping past index 0 exits
// nav mode and restores the saved draft (which may be empty).
func (m *Model) directHistoryDown() bool {
	if m.histDirectIdx < 0 {
		// Not navigating — let textarea handle ↓ as cursor move.
		return false
	}
	if m.histDirectIdx == 0 {
		// Past the newest — exit nav mode, restore draft.
		m.histDirectIdx = -1
		m.input.SetValue(m.histDirectDraft)
		m.input.CursorEnd()
		m.histDirectDraft = ""
		return true
	}
	m.histDirectIdx--
	m.input.SetValue(m.histAll[m.histDirectIdx])
	m.input.CursorEnd()
	return true
}

// resetDirectHistoryNav drops the nav-mode flag without touching the
// input. Called whenever the user types a printable rune or submits,
// so the next ↑ starts a fresh walk from the (now-mutated) draft.
func (m *Model) resetDirectHistoryNav() {
	m.histDirectIdx = -1
	m.histDirectDraft = ""
}
