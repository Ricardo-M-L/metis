// Package tasks persists per-session todo lists under ~/.metis/tasks/.
//
// Built-in tools (TodoWrite / TodoRead) and runtime helpers both read
// from this package — splitting it out from internal/runtime breaks an
// otherwise-circular dependency (runtime → tools → builtin → runtime
// would cycle if Todo persistence lived in runtime).
package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// home resolves the metis home dir without importing config (to keep
// internal/tasks a leaf package). Mirror of internal/auth's home() —
// the rule is short and stable enough that copying is fine.
func home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	hd, _ := os.UserHomeDir()
	return filepath.Join(hd, ".metis")
}

// Dir returns the tasks directory.
func Dir() string {
	return filepath.Join(home(), "tasks")
}

// Item is one row in a session's task list.
type Item struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`   // pending | in_progress | completed
	Priority  string    `json:"priority"` // low | medium | high
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List is the on-disk task list for one session.
type List struct {
	SessionID string `json:"session"`
	Items     []Item `json:"items"`
}

var mu sync.Mutex

func path(sessionID string) string {
	if sessionID == "" {
		sessionID = "default"
	}
	return filepath.Join(Dir(), sessionID+".json")
}

// Load returns the list for sessionID. Missing file → empty list, not
// error (the LLM's first TodoWrite call is the implicit creation).
func Load(sessionID string) (*List, error) {
	tl := &List{SessionID: sessionID}
	b, err := os.ReadFile(path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return tl, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, tl); err != nil {
		return nil, fmt.Errorf("tasks: parse %s: %w", path(sessionID), err)
	}
	if tl.SessionID == "" {
		tl.SessionID = sessionID
	}
	return tl, nil
}

// Save rewrites the task file. 0o644 — task content is already shown
// inline so it's not secret.
func Save(tl *List) error {
	if tl == nil {
		return fmt.Errorf("tasks: nil list")
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(tl.SessionID), b, 0o644)
}

// Upsert creates or updates one item. Match priority: explicit ID >
// content string > append new. Status / Priority preserved when caller
// leaves them empty.
func Upsert(sessionID string, in Item) (Item, error) {
	mu.Lock()
	defer mu.Unlock()
	tl, err := Load(sessionID)
	if err != nil {
		return Item{}, err
	}
	now := time.Now()

	idx := -1
	if in.ID != "" {
		for i := range tl.Items {
			if tl.Items[i].ID == in.ID {
				idx = i
				break
			}
		}
	}
	if idx < 0 && in.Content != "" {
		// Exact-match pass first (fastest, lossless).
		for i := range tl.Items {
			if tl.Items[i].Content == in.Content {
				idx = i
				break
			}
		}
	}
	if idx < 0 && in.Content != "" {
		// Normalised-match fallback (2026-05-21, image #50 repro):
		// model created "Cluster 2: Wire Protocol (...)" then later
		// re-wrote it as "2. Wire Protocol (...)" without passing
		// id; the bare equality check missed the dup and metis
		// appended a second entry. normalizeTaskContent strips
		// numbering prefixes ("Cluster N:" / "N." / "N)") and
		// trailing ellipsis so the dedup catches these re-wordings
		// without merging genuinely distinct tasks like "Cluster
		// 1: Foundation" vs "Cluster 1: exception, ..." (those
		// normalise differently because the meaningful body
		// differs).
		want := normalizeTaskContent(in.Content)
		for i := range tl.Items {
			if normalizeTaskContent(tl.Items[i].Content) == want {
				idx = i
				break
			}
		}
	}

	if idx >= 0 {
		tl.Items[idx].Content = in.Content
		if in.Status != "" {
			tl.Items[idx].Status = in.Status
		}
		if in.Priority != "" {
			tl.Items[idx].Priority = in.Priority
		}
		tl.Items[idx].UpdatedAt = now
		out := tl.Items[idx]
		return out, Save(tl)
	}

	if in.ID == "" {
		in.ID = uuid.NewString()[:8]
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	if in.Priority == "" {
		in.Priority = "medium"
	}
	in.CreatedAt = now
	in.UpdatedAt = now
	tl.Items = append(tl.Items, in)
	tl.SessionID = sessionID
	return in, Save(tl)
}

// ReplaceAll replaces the entire task list with `items` — the Claude
// Code TodoWrite semantics (declarative full-list replace). Because the
// caller restates EVERY task with its current status on each call,
// earlier tasks can't be silently left stuck mid-status. This is the
// fix for the 2026-06-14 bug where a model finished three verify tasks,
// marked only the final summary completed, and left the three showing
// "in progress" — the single-task Upsert form makes that omission easy,
// the full-list form makes it impossible.
//
// Existing tasks are matched (id → exact content → normalised content,
// the same ladder as Upsert) so a task keeps its id + CreatedAt across
// calls instead of churning a fresh id every time.
func ReplaceAll(sessionID string, items []Item) (*List, error) {
	mu.Lock()
	defer mu.Unlock()
	old, err := Load(sessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	// claimed tracks old IDs already matched in THIS call so two new
	// items whose content normalises to the same thing don't both claim
	// the same old id (which would produce duplicate ids). The second
	// one falls through to a fresh id.
	claimed := make(map[string]bool, len(items))
	out := make([]Item, 0, len(items))
	for _, in := range items {
		if strings.TrimSpace(in.Content) == "" {
			continue
		}
		if prev := findExisting(old, in); prev != nil && !claimed[prev.ID] {
			in.ID = prev.ID
			in.CreatedAt = prev.CreatedAt
			claimed[prev.ID] = true
		} else {
			if in.ID == "" || claimed[in.ID] {
				in.ID = uuid.NewString()[:8]
			}
			in.CreatedAt = now
			claimed[in.ID] = true
		}
		if in.Status == "" {
			in.Status = "pending"
		}
		if in.Priority == "" {
			in.Priority = "medium"
		}
		in.UpdatedAt = now
		out = append(out, in)
	}
	tl := &List{SessionID: sessionID, Items: out}
	return tl, Save(tl)
}

// findExisting locates the item in `old` matching `in` by the same
// ladder Upsert uses: id, then exact content, then normalised content.
// Returns nil when nothing matches.
func findExisting(old *List, in Item) *Item {
	if old == nil {
		return nil
	}
	if in.ID != "" {
		for i := range old.Items {
			if old.Items[i].ID == in.ID {
				return &old.Items[i]
			}
		}
	}
	for i := range old.Items {
		if old.Items[i].Content == in.Content {
			return &old.Items[i]
		}
	}
	want := normalizeTaskContent(in.Content)
	for i := range old.Items {
		if normalizeTaskContent(old.Items[i].Content) == want {
			return &old.Items[i]
		}
	}
	return nil
}

// CurrentSessionID is set by setupRuntime once a session exists. The
// TodoWrite tool reads it lazily because tools are stateless and built
// before the session id is available.
var (
	currMu  sync.RWMutex
	current string
)

// SetCurrentSessionID is called by setupRuntime once a session id
// exists (either freshly created or resumed). Empty input is allowed
// — defaults to per-session "default" file.
func SetCurrentSessionID(id string) {
	currMu.Lock()
	defer currMu.Unlock()
	current = id
}

// CurrentSessionID returns the runtime-set session id (empty before
// setup completes).
func CurrentSessionID() string {
	currMu.RLock()
	defer currMu.RUnlock()
	return current
}

// normalizeTaskContent strips superficial decorations that often
// differ between create + update calls when the model forgets to
// pass the original id:
//
//   - Numbering prefixes ("Cluster N:", "Phase N:", "N.", "N)")
//   - Leading/trailing whitespace
//   - Trailing ellipsis (… or ...) — a common LLM artefact from
//     truncated rendering of an earlier listing
//   - Multiple internal spaces collapsed to one
//
// The goal is to catch "Cluster 2: Wire Protocol" ≡
// "2. Wire Protocol" as the same task while keeping genuinely
// distinct items ("Cluster 1: Foundation" vs "Cluster 1: exception
// + constant") separate (their bodies differ post-normalization).
//
// Lowercase comparison is intentional: the model sometimes
// re-cases its own task names mid-stream ("CLuster" vs "Cluster").
// Lossy enough to catch dupes; specific enough not to over-merge.
func normalizeTaskContent(s string) string {
	t := strings.TrimSpace(s)
	t = stripNumberingPrefix(t)
	t = strings.TrimRight(t, ". \t…")
	// Collapse any run of whitespace to a single space.
	var b strings.Builder
	b.Grow(len(t))
	lastSpace := false
	for _, r := range t {
		if r == ' ' || r == '\t' || r == '\n' {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// stripNumberingPrefix removes a leading numbered/labelled prefix
// the model commonly attaches to task items. Handles:
//
//	"Cluster 1: foo"        → "foo"
//	"Phase 2 - foo"         → "foo"
//	"2. foo"                → "foo"
//	"2) foo"                → "foo"
//	"[2] foo"               → "foo"
//
// Returns the original string unchanged when no recognised prefix
// matches — so a task whose real content starts with "Wire Protocol"
// is left alone.
func stripNumberingPrefix(s string) string {
	// Try "Cluster N:" / "Phase N:" / "Step N:" patterns.
	for _, word := range []string{"Cluster", "Phase", "Step", "Part", "Stage"} {
		if rest, ok := tryStripWordNumberSep(s, word); ok {
			return strings.TrimSpace(rest)
		}
	}
	// Try plain "N. " / "N) " / "[N] " numbering.
	if rest, ok := tryStripPlainNumber(s); ok {
		return strings.TrimSpace(rest)
	}
	return s
}

func tryStripWordNumberSep(s, word string) (string, bool) {
	lower := strings.ToLower(s)
	wLower := strings.ToLower(word)
	if !strings.HasPrefix(lower, wLower+" ") {
		return "", false
	}
	rest := s[len(word)+1:]
	rest = strings.TrimLeft(rest, " ")
	// Expect a digit run next.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return "", false
	}
	rest = rest[end:]
	// Eat the separator: ':' / '-' / '—' / '.'
	rest = strings.TrimLeft(rest, ":-—. )]")
	return rest, true
}

func tryStripPlainNumber(s string) (string, bool) {
	// Optional leading '[' for "[N] ..." form.
	bracketed := strings.HasPrefix(s, "[")
	rest := s
	if bracketed {
		rest = rest[1:]
	}
	// Scan a digit run.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return "", false
	}
	tail := rest[end:]
	// Must be followed by a separator that signals "this was a
	// numbering label" — otherwise the number is part of the
	// content (e.g. "5-second timeout"). Accept ". " / ") " /
	// "] " / ": ".
	for _, sep := range []string{". ", ") ", "] ", ": "} {
		if strings.HasPrefix(tail, sep) {
			return tail[len(sep):], true
		}
	}
	return "", false
}
