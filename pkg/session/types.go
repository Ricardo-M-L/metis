// Package session exposes the public Session record types.
//
// Plugin authors typically read session JSONL files to build dashboards
// (recent activity, cost analytics, search across past chats) or write
// custom export formatters (markdown / HTML / 飞书 doc). This package
// provides the data shapes; the file format itself is documented in
// internal/session.
//
// The persistence layer (Store, NewStore, Append, Load) stays internal —
// 3rd parties usually want read-only access via the JSONL files directly,
// not full mutation capability that could race with the running agent.
package session

import "time"

// Header is the session-level metadata persisted as the first entry of
// every JSONL session file. Subsequent SetTitle calls append a partial
// header; the on-load merge is documented in internal/session.
type Header struct {
	ID          string      `json:"id"`
	CreatedAt   time.Time   `json:"created_at"`
	Provider    string      `json:"provider,omitempty"`
	Model       string      `json:"model"`
	System      string      `json:"system,omitempty"`
	WorkDir     string      `json:"work_dir,omitempty"`
	Mode        string      `json:"mode,omitempty"`
	AlwaysAllow []SavedRule `json:"always_allow,omitempty"`
	// ClearAlwaysAllow is an append-only tombstone used when a later header
	// must remove every previously persisted interactive grant.
	ClearAlwaysAllow bool   `json:"clear_always_allow,omitempty"`
	Title            string `json:"title,omitempty"`
	// ForkedFrom — non-nil when this session was created via /branch. Stores
	// the parent session ID so a viewer / `metis sessions list` can render
	// the lineage. Mirrors claude-code's `forkedFrom` metadata; the link is
	// one-way (child → parent) by design — discovering branches of a parent
	// session means scanning all sessions, which is fine at metis's scale.
	ForkedFrom *ForkRef `json:"forked_from,omitempty"`

	// SubAgentOf — non-empty when this JSONL is a sub-agent's transcript
	// rather than a top-level chat session (G.4, 2026-05-12). Holds the
	// parent agent's session ID so `metis sessions list` can group
	// sub-agents under their spawner. Empty for ordinary sessions.
	// Distinct from ForkedFrom which is for /branch (full history copy);
	// SubAgentOf is for live sub-agent persistence so `/agents resume`
	// can rebuild a paused sub-agent.
	SubAgentOf string `json:"sub_agent_of,omitempty"`

	// TeammateName — when this transcript was a named teammate, the
	// name passed to Agent({name: "..."}). Empty for anonymous spawns.
	// Used by `/agents resume` UI to show "resume alice?" instead of
	// raw agent_id, and by SubAgentList to label history entries.
	TeammateName string `json:"teammate_name,omitempty"`
}

// ForkRef points back at the parent session of a /branch'd session.
// MessageCount is the parent's message count at fork time, useful for
// downstream tooling that wants to highlight where the histories diverge
// (a branch made early-on vs after dozens of turns).
type ForkRef struct {
	SessionID    string `json:"session_id"`
	MessageCount int    `json:"message_count,omitempty"`
}

// SavedRule mirrors permission.Rule but lives here to break the import
// cycle (permission can't depend on session). The two are converted at
// the boundary in cmd/metis.
type SavedRule struct {
	Tool   string `json:"tool"`
	Match  string `json:"match,omitempty"`
	Verb   int    `json:"verb"` // permission.Decision int
	Source string `json:"source,omitempty"`
}

// ListEntry is one row of session.Store.List() output: enough to render
// a session-picker without loading the full transcript.
type ListEntry struct {
	ID        string
	CreatedAt time.Time
	Model     string
	Title     string // empty when /title hasn't been called for this session
	Bytes     int64
}
