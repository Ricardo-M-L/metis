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
// they'd move the cursor between rows). Three cases qualify:
//
//  1. Input is empty.
//  2. Input was last set by direct-history nav (user hasn't typed since
//     loading a previous prompt) — detected by histDirectIdx ≥ 0.
//  3. Cursor is at the very start of the input (line 0, column 0) —
//     claude-code parity. User flow: Home → ↑ loads previous prompt.
//     Required for the "把光标放到最开始的位置然后按向上箭头切换历史"
//     workflow (2026-05-16 user report, screenshot 32). Without this,
//     a non-empty single- or multi-line input swallowed the ↑ entirely
//     even after Home, because the cursor was already at col 0 so
//     CursorStart was a no-op and textarea LineUp had no row above to
//     land on.
//
// A multi-line input that the user is actively editing on a later row
// (or anywhere past column 0 of row 0) still falls through, so ↑
// continues to move the cursor between rows / jumps to col 0 as before.
func (m *Model) directHistoryEligible() bool {
	if m.histDirectIdx >= 0 {
		return true
	}
	if m.input.Value() == "" {
		return true
	}
	return m.input.Line() == 0 && m.input.Column() == 0
}

// loadDirectHistory lazy-loads ~/.metis/history.jsonl into m.histAll on
// first use. Same backing data as the Ctrl+R overlay, so opening that
// overlay later sees identical entries (no double-load).
//
// Scope: filtered to the current session (m.sessionID) — user
// expectation 2026-05-16 "向上向下箭头出来的是当前会话的吧不会出来
// 别的会话的吧". Cross-session prompts are still reachable via the
// Ctrl+R fuzzy-search overlay, which has its own load path.
//
// Fallback: when m.sessionID is empty (REPL-fallback mode or auth-
// wizard exit before a session was bound), don't filter — otherwise
// histAll would be empty and ↑ would be a silent no-op even though
// the user does have prior prompts on disk.
func (m *Model) loadDirectHistory() {
	if m.histAll != nil {
		return
	}
	entries, _ := runtime.LoadRecentHistory(500)
	filterToSession := m.sessionID != ""
	seen := map[string]bool{}
	for _, e := range entries {
		if filterToSession && e.SessionID != m.sessionID {
			continue
		}
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
