package builtin

// sessions.go — cross-session query tool (DSH session-query parity).
//
// History searches the CURRENT session's transcript; Sessions searches
// ACROSS every session in the store. The model gets three operations:
//
//   - list:    recent sessions (id, title, updated, message count)
//   - search:  BM25 rank over every message of every session + titles,
//              deduped to a per-session rollup so one chatty session
//              can't flood the result page
//   - read:    one session's digest — header + a bounded window of
//              messages (around an index when given, else first+last)
//
// The tool takes injected closures (listFn/loadFn) rather than a
// session.Store directly, keeping this package free of a session-store
// dependency — same convention as History.
//
// Permission model: the store dir belongs to the user's own metis
// install; the tool is read-only over it, so no permission gate beyond
// the existing Bash/Read filesystem story is required.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const (
	sessionsDefaultLimit  = 8
	sessionsMaxScan       = 100 // most-recent sessions scanned per search
	sessionsDigestChars   = 6000
	sessionsReadAroundDef = 4
	sessionsReadWindowDef = 10
)

// SessionInfo is the catalog row the injected listFn returns.
type SessionInfo struct {
	ID           string
	Title        string
	Model        string
	UpdatedAt    time.Time
	MessageCount int
}

// Sessions is the LLM-facing cross-session query tool.
type Sessions struct {
	tools.BaseTool
	listFn func(limit int) ([]SessionInfo, error)
	loadFn func(id string) ([]llm.Message, error)
}

// NewSessions builds the tool. Both closures are injected by runtime and
// wrap session.Store.List/Load. nil closures → tool reports "unavailable".
func NewSessions(listFn func(limit int) ([]SessionInfo, error), loadFn func(id string) ([]llm.Message, error)) *Sessions {
	return &Sessions{listFn: listFn, loadFn: loadFn}
}

func (Sessions) Name() string { return "Sessions" }

func (Sessions) Description() string {
	return `Query OTHER sessions in this metis install — the shared memory of every past conversation.

operation "list": recent sessions with id, title, updated time, message count.
operation "search": full-text rank across every message of every stored session (plus session titles). Returns the top sessions with matching snippets — useful for "we discussed X before, find it".
operation "read": one session's digest — resolve by id (or unique prefix / title substring), optionally around a transcript index from a prior search.

Typical flow: search a keyword → note the session id + index → read that session around the index. Use this BEFORE re-asking the user something a past session already answered.`
}

func (Sessions) ShortDescription() string {
	return `Search and read OTHER past sessions (cross-session memory). operation: "list", "search" {query}, or "read" {session, index?}.`
}

func (Sessions) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"operation"},
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "search", "read"},
				"description": "list: recent sessions. search: rank across all sessions. read: one session's digest.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search terms (operation=search).",
			},
			"session": map[string]any{
				"type":        "string",
				"description": "Session id, unique id prefix, or title substring (operation=read).",
			},
			"index": map[string]any{
				"type":        "integer",
				"description": "Transcript index to center the read window on (operation=read, optional; from a prior search hit).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results (default 8).",
			},
		},
	}
}

func (s Sessions) IsEnabled() bool { return s.listFn != nil && s.loadFn != nil }

func (Sessions) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}

func (Sessions) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (s Sessions) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if !s.IsEnabled() {
		return &tools.Result{Output: "Sessions: unavailable (no session store configured).", IsError: true}, nil
	}
	op, _ := in["operation"].(string)
	switch op {
	case "list":
		return s.opList(in), nil
	case "search":
		return s.opSearch(in), nil
	case "read":
		return s.opRead(in), nil
	default:
		return &tools.Result{Output: `Sessions: ` + "`operation`" + ` must be "list", "search", or "read".`, IsError: true}, nil
	}
}

func (s Sessions) opList(in map[string]any) *tools.Result {
	limit := intArg(in, "limit", sessionsDefaultLimit)
	if limit <= 0 || limit > 50 {
		limit = sessionsDefaultLimit
	}
	infos, err := s.listFn(limit)
	if err != nil {
		return &tools.Result{Output: "Sessions list: " + err.Error(), IsError: true}
	}
	if len(infos) == 0 {
		return &tools.Result{Output: "Sessions list: no stored sessions."}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Recent sessions (%d):\n", len(infos)))
	for _, si := range infos {
		title := si.Title
		if title == "" {
			title = "(untitled)"
		}
		b.WriteString(fmt.Sprintf("- %s · %q · %d msgs · updated %s\n",
			si.ID, title, si.MessageCount, si.UpdatedAt.Format("2006-01-02 15:04")))
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}
}

func (s Sessions) opSearch(in map[string]any) *tools.Result {
	query := strings.TrimSpace(strArg(in, "query"))
	if query == "" {
		return &tools.Result{Output: "Sessions search: `query` is required.", IsError: true}
	}
	limit := intArg(in, "limit", sessionsDefaultLimit)
	if limit <= 0 || limit > 25 {
		limit = sessionsDefaultLimit
	}
	infos, err := s.listFn(sessionsMaxScan)
	if err != nil {
		return &tools.Result{Output: "Sessions search: " + err.Error(), IsError: true}
	}
	type sessHit struct {
		info SessionInfo
		best float64
		idx  int
		snip string
		role string
	}
	bySess := map[string]*sessHit{}
	var docs []*memory.BM25Doc
	for _, si := range infos {
		msgs, err := s.loadFn(si.ID)
		if err != nil {
			continue
		}
		// Title doc: id ∅ -1 — a title match should surface the session
		// even when its messages use different wording.
		title := si.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		docs = append(docs, memory.NewBM25Doc(si.ID+"\x00t", title))
		if _, ok := bySess[si.ID]; !ok {
			bySess[si.ID] = &sessHit{info: si, idx: -1}
		}
		for i, m := range msgs {
			text, _ := messageSearchText(m)
			if strings.TrimSpace(text) == "" {
				continue
			}
			docs = append(docs, memory.NewBM25Doc(fmt.Sprintf("%s\x00%d", si.ID, i), text))
		}
	}
	ranked := memory.BM25FRank(query, docs)
	for _, r := range ranked {
		id, idx, ok := splitDocID(r.ID)
		if !ok {
			continue
		}
		h, ok := bySess[id]
		if !ok {
			continue
		}
		if r.Score > h.best {
			h.best = r.Score
			h.idx = idx
			if idx >= 0 {
				if msgs, err := s.loadFn(id); err == nil && idx < len(msgs) {
					text, _ := messageSearchText(msgs[idx])
					h.snip = snippet(text, historySnippetChars)
					h.role = string(msgs[idx].Role)
				}
			} else {
				h.snip = snippet(h.info.Title, historySnippetChars)
				h.role = "title"
			}
		}
	}
	var hits []*sessHit
	for _, h := range bySess {
		if h.best > 0 {
			hits = append(hits, h)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].best > hits[j].best })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	if len(hits) == 0 {
		return &tools.Result{Output: fmt.Sprintf("Sessions search: no matches for %q across %d sessions.", query, len(infos))}
	}
	type outHit struct {
		Session string  `json:"session"`
		Title   string  `json:"title"`
		Index   int     `json:"index"`
		Role    string  `json:"role"`
		Score   float64 `json:"score"`
		Snippet string  `json:"snippet"`
		Msgs    int     `json:"msgs"`
	}
	out := make([]outHit, 0, len(hits))
	for _, h := range hits {
		title := h.info.Title
		if title == "" {
			title = "(untitled)"
		}
		out = append(out, outHit{Session: h.info.ID, Title: title, Index: h.idx, Role: h.role, Score: h.best, Snippet: h.snip, Msgs: h.info.MessageCount})
	}
	body, _ := json.Marshal(map[string]any{"sessions_scanned": len(infos), "matches": out})
	return &tools.Result{Output: string(body)}
}

func (s Sessions) opRead(in map[string]any) *tools.Result {
	ref := strings.TrimSpace(strArg(in, "session"))
	if ref == "" {
		return &tools.Result{Output: "Sessions read: `session` (id, prefix, or title substring) is required.", IsError: true}
	}
	infos, err := s.listFn(sessionsMaxScan)
	if err != nil {
		return &tools.Result{Output: "Sessions read: " + err.Error(), IsError: true}
	}
	id := ResolveSessionRef(infos, ref)
	if id == "" {
		return &tools.Result{Output: fmt.Sprintf("Sessions read: %q does not uniquely match a session (checked %d). Use `list` to see ids/titles.", ref, len(infos)), IsError: true}
	}
	msgs, err := s.loadFn(id)
	if err != nil {
		return &tools.Result{Output: "Sessions read: " + err.Error(), IsError: true}
	}
	var info SessionInfo
	for _, si := range infos {
		if si.ID == id {
			info = si
			break
		}
	}
	idx := intArg(in, "index", -1)
	digest := SessionDigest(info, msgs, idx, sessionsDigestChars)
	return &tools.Result{Output: digest}
}

// SessionDigest renders a bounded, model-friendly digest of one session:
// header line + a message window. idx >= 0 centers the window there
// (around); otherwise first + last slices are shown with a gap marker.
// Exported so the TUI's @session: expansion reuses the exact same shape.
func SessionDigest(info SessionInfo, msgs []llm.Message, idx, maxChars int) string {
	if maxChars <= 0 {
		maxChars = sessionsDigestChars
	}
	var b strings.Builder
	title := info.Title
	if title == "" {
		title = "(untitled)"
	}
	b.WriteString(fmt.Sprintf("[session %s · %q · %s · %s · %d messages]\n",
		info.ID, title, info.Model, info.UpdatedAt.Format("2006-01-02 15:04"), len(msgs)))

	writeMsg := func(i int) {
		if i < 0 || i >= len(msgs) {
			return
		}
		text, _ := messageSearchText(msgs[i])
		if strings.TrimSpace(text) == "" {
			return
		}
		line := snippet(text, 700)
		b.WriteString(fmt.Sprintf("#%d %s: %s\n", i, msgs[i].Role, line))
	}

	remaining := maxChars - b.Len()
	if idx >= 0 && idx < len(msgs) {
		around := sessionsReadAroundDef
		for i := idx - around; i <= idx+around && remaining > 0; i++ {
			before := b.Len()
			writeMsg(i)
			remaining -= b.Len() - before
		}
	} else {
		w := sessionsReadWindowDef
		// head
		for i := 0; i < w && i < len(msgs) && remaining > 0; i++ {
			before := b.Len()
			writeMsg(i)
			remaining -= b.Len() - before
		}
		if len(msgs) > 2*w {
			b.WriteString("… (middle omitted) …\n")
			remaining -= 24
		}
		// tail
		for i := len(msgs) - w; i < len(msgs) && remaining > 0; i++ {
			if i < 0 {
				continue
			}
			before := b.Len()
			writeMsg(i)
			remaining -= b.Len() - before
		}
	}
	out := b.String()
	if len(out) > maxChars {
		out = out[:maxChars] + "\n… (digest truncated)"
	}
	return strings.TrimRight(out, "\n")
}

// ResolveSessionRef matches ref against the catalog: exact id, then
// unique id prefix, then unique title substring (case-insensitive).
// Empty string when ambiguous or unmatched. Exported so the TUI's
// @session: expansion agrees with the tool's read op on semantics.
func ResolveSessionRef(infos []SessionInfo, ref string) string {
	// Slug-normalized match target: users type @session:kafka-lag while
	// the stored title says "kafka lag" — dashes/underscores stand in
	// for spaces in the token syntax.
	lower := slugNorm(ref)
	var prefix, title []string
	for _, si := range infos {
		if si.ID == ref {
			return si.ID
		}
		if strings.HasPrefix(si.ID, ref) {
			prefix = append(prefix, si.ID)
		}
		if si.Title != "" && strings.Contains(slugNorm(si.Title), lower) {
			title = append(title, si.ID)
		}
	}
	if len(prefix) == 1 {
		return prefix[0]
	}
	if len(title) == 1 {
		return title[0]
	}
	return ""
}

func splitDocID(id string) (string, int, bool) {
	i := strings.LastIndexByte(id, 0)
	if i < 0 {
		return "", 0, false
	}
	suffix := id[i+1:]
	if suffix == "t" {
		return id[:i], -1, true // title doc
	}
	idx := 0
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		idx = idx*10 + int(c-'0')
	}
	return id[:i], idx, true
}

func strArg(in map[string]any, key string) string {
	v, _ := in[key].(string)
	return v
}

// slugNorm lowercases and treats `-` and `_` as spaces so slug-style
// references match natural-language titles.
func slugNorm(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	return strings.Join(strings.Fields(s), " ")
}
