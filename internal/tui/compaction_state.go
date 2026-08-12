package tui

import "github.com/Ricardo-M-L/metis/internal/session"

// resetTokenUsageAfterCompaction drops provider usage captured before a
// successful context rewrite. Those counters describe the request that caused
// compaction, not the compacted history, so retaining them makes the status bar
// report the old window indefinitely. The user-facing /usage command remains
// the authoritative place for provider quota/usage; /cost intentionally
// restarts at the compact boundary.
func resetTokenUsageAfterCompaction(tokens *tokenTracker, store *session.Store, sessionID string) {
	if tokens != nil {
		tokens.Reset()
	}
	// Do not resurrect pre-compact /cost counters on the next resume.
	if store != nil && sessionID != "" {
		_ = store.WriteCost(sessionID, session.CostSnapshot{})
	}
}

// resetTokenUsageAfterCompaction updates the persistent plain REPL directly,
// or delegates to the live Bubble Tea model when this REPL is only an asREPL
// bridge carrying a value-copy of tokenTracker.
func (r *REPL) resetTokenUsageAfterCompaction() {
	if r == nil {
		return
	}
	if r.ResetTokenUsageAfterCompaction != nil {
		r.ResetTokenUsageAfterCompaction()
		return
	}
	resetTokenUsageAfterCompaction(&r.totalTokens, r.Session, r.SessionID)
}
