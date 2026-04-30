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
	Model       string      `json:"model"`
	System      string      `json:"system,omitempty"`
	WorkDir     string      `json:"work_dir,omitempty"`
	Mode        string      `json:"mode,omitempty"`
	AlwaysAllow []SavedRule `json:"always_allow,omitempty"`
	Title       string      `json:"title,omitempty"`
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
