package tui

import "github.com/Ricardo-M-L/metis/internal/session"

// resetTokenUsageAfterCompaction drops only the most-recent provider usage
// captured before a successful context rewrite. Those last-call fields no
// longer describe the active window; cumulative token/cost counters still
// describe real spend and must survive both compaction and resume.
func resetTokenUsageAfterCompaction(tokens *tokenTracker, store *session.Store, sessionID string) {
	if tokens != nil {
		tokens.ResetLast()
	}
	// Kept in the signature for REPL/Model call-site compatibility. In
	// particular, do not rewrite the persisted CostSnapshot here.
	_ = store
	_ = sessionID
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
