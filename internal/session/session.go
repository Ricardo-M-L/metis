// Package session persists conversations to JSONL on disk so they can
// be resumed or audited. One file per session under cfg.Session.Dir.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
	"github.com/google/uuid"
)

// Header records what's needed to resume a session faithfully. Inspired by
// Claude Code's `resumeAgent.ts` whitelist approach: every field listed here
// is restored on `--resume`; everything else (cron jobs, transient
// loop-detector state) is intentionally NOT in the session and stays in its
// own store. Adding a new field here means it survives across `metis run
// --resume <id>` invocations.
// Header / SavedRule re-export from pkg/session. In-tree code keeps using
// `session.Header` etc.; plugin authors import pkg/session directly.
type (
	Header    = pubsess.Header
	SavedRule = pubsess.SavedRule
)

type Entry struct {
	Type     string        `json:"type"` // "header" | "message" | "history_replace"
	Header   *Header       `json:"header,omitempty"`
	Message  *llm.Message  `json:"message,omitempty"`
	Messages []llm.Message `json:"messages,omitempty"`
}

type Store struct {
	Dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) NewSessionID() string { return uuid.NewString() }

func (s *Store) path(id string) string {
	// Path-traversal guard: filepath.Base strips any directory parts, so a
	// crafted/imported id like "../../tmp/evil" collapses to "evil" and can
	// never escape s.Dir. Legit ids (uuid / timestamp) have no separators,
	// so this is a no-op for them.
	return filepath.Join(s.Dir, filepath.Base(id)+".jsonl")
}

func (s *Store) WriteHeader(id, model, system string) error {
	h := &Header{ID: id, CreatedAt: time.Now(), Model: model, System: system}
	return s.append(id, Entry{Type: "header", Header: h})
}

// WriteHeaderFull writes an extended header that includes the resume-aware
// fields (work dir, permission mode, always-allow rules). Use this when you
// have a fully-configured runtime ready to persist; WriteHeader stays for
// callers that only know name/model/system at session creation time.
func (s *Store) WriteHeaderFull(h Header) error {
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now()
	}
	return s.append(h.ID, Entry{Type: "header", Header: &h})
}

func (s *Store) AppendMessage(id string, m llm.Message) error {
	return s.append(id, Entry{Type: "message", Message: &m})
}

// ReplaceHistoryAndMark appends a full-history snapshot to the session log.
// Load treats this entry as a replacement for every earlier message entry,
// while later message entries continue to append normally. Keeping the JSONL
// append-only preserves atomic line writes and concurrent-reader behaviour,
// but makes destructive in-memory operations (/clear, /undo, /rewind and
// compaction) durable instead of allowing old messages to reappear on resume.
//
// The cursor advances only after the snapshot line reaches disk. A failed
// write therefore remains retryable: the next AppendHistoryTail still sees
// the old anchor and emits the replacement again.
func (s *Store) ReplaceHistoryAndMark(id string, history []llm.Message, cursor *HistoryCursor) error {
	if cursor == nil {
		return errors.New("replace history: nil cursor")
	}
	if s == nil || id == "" {
		cursor.Mark(history)
		return nil
	}
	if err := s.append(id, Entry{Type: "history_replace", Messages: history}); err != nil {
		return fmt.Errorf("replace history: %w", err)
	}
	cursor.Mark(history)
	return nil
}

func (s *Store) append(id string, e Entry) error {
	// 2026-05-22: switched from json.Encoder to manual Marshal +
	// single Write to guarantee atomic-per-line append. json.Encoder
	// usually writes once-per-Encode but it's not contractually
	// guaranteed; for safety we marshal in memory first so the
	// underlying file.Write is a single syscall (the kernel either
	// commits the whole buffer or none of it on a clean signal kill —
	// torn writes only happen on power loss / kernel panic, which is
	// out of scope for a user-space CLI).
	//
	// Pairs with Load's tolerant trailing-line handling: even if a
	// torn write does happen, the resume path now drops the bad
	// trailing line instead of aborting the whole session.
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(s.path(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

func (s *Store) Load(id string) (*Header, []llm.Message, error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var hdr *Header
	var msgs []llm.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	// 2026-05-22: tolerate a corrupted trailing line. Pre-fix any
	// JSON decode error aborted Load and the session was effectively
	// unrecoverable. Real-world torn-write scenarios (SIGKILL during
	// write, power loss) almost always corrupt only the LAST
	// in-flight line, so we buffer the line index and surface the
	// failure only if a mid-file line is bad (where corruption
	// implies a real data-integrity problem worth surfacing).
	lineNum := 0
	var pendingBadLine string
	for sc.Scan() {
		lineNum++
		// If the previous iteration deferred a bad line as
		// "might be the last", a successful Scan here proves it
		// was mid-file — surface that as a hard error.
		if pendingBadLine != "" {
			return nil, nil, fmt.Errorf("decode session entry at line %d: %s", lineNum-1, pendingBadLine)
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// Defer the failure. If sc.Scan returns false next
			// (EOF), this WAS the trailing line — skip it. If
			// another line follows, this was mid-file corruption
			// and we abort above.
			pendingBadLine = err.Error()
			continue
		}
		switch e.Type {
		case "header":
			// Merge subsequent headers onto the first so /title and
			// future incremental edits survive without rewriting the
			// whole JSONL file. CreatedAt is sticky to the original.
			if hdr == nil {
				hdr = e.Header
			} else {
				mergeHeader(hdr, e.Header)
			}
		case "message":
			if e.Message != nil {
				msgs = append(msgs, *e.Message)
			}
		case "history_replace":
			// A nil/omitted messages field is a valid empty snapshot and is
			// how /clear invalidates every earlier message in the JSONL.
			msgs = append([]llm.Message(nil), e.Messages...)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if pendingBadLine != "" {
		// Last line was corrupted (likely torn write from a prior
		// crash). Log to stderr so the user knows, but allow
		// resume to proceed with the clean prefix.
		fmt.Fprintf(os.Stderr, "session %s: dropped corrupted trailing line %d (%s); resuming with clean prefix\n",
			id, lineNum, pendingBadLine)
	}
	if hdr == nil {
		return nil, nil, errors.New("session header missing")
	}
	canonicalizeToolInputs(msgs)
	return hdr, msgs, nil
}

// canonicalizeToolInputs heals transcripts written before name-only tool calls
// were normalised at stream ingestion. ContentBlock.input is intentionally
// `omitempty`, so an empty object and a legacy nil both appear as an omitted
// field on disk; after decoding, however, every tool_use must expose an object
// to provider adapters and tool dispatch. Keep this repair in Load so resume,
// branch, and imported-session paths all get the same safe representation.
func canonicalizeToolInputs(messages []llm.Message) {
	for i := range messages {
		for j := range messages[i].Content {
			block := &messages[i].Content[j]
			if block.Type == "tool_use" && block.ToolInput == nil {
				block.ToolInput = map[string]any{}
			}
		}
	}
}

// mergeHeader applies non-zero fields from src onto dst. The caller decides
// what counts as "set" — we treat the empty string / nil slice as "leave
// alone" so partial header updates (e.g. SetTitle writing only Title) don't
// clobber the original Model / System / etc.
func mergeHeader(dst *Header, src *Header) {
	if src == nil {
		return
	}
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.System != "" {
		dst.System = src.System
	}
	if src.WorkDir != "" {
		dst.WorkDir = src.WorkDir
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
	if src.ClearAlwaysAllow {
		dst.AlwaysAllow = nil
		dst.ClearAlwaysAllow = false
	} else if len(src.AlwaysAllow) > 0 {
		dst.AlwaysAllow = src.AlwaysAllow
	}
	if src.SubAgentOf != "" {
		dst.SubAgentOf = src.SubAgentOf
	}
	if src.TeammateName != "" {
		dst.TeammateName = src.TeammateName
	}
}

// ListEntry re-export — same alias pattern.
type ListEntry = pubsess.ListEntry

// List enumerates available sessions, newest first.
func (s *Store) List(limit int) ([]ListEntry, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []ListEntry
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".jsonl")]
		hdr, _, err := s.LoadHeader(id)
		if err != nil {
			continue
		}
		out = append(out, ListEntry{
			ID: id, CreatedAt: hdr.CreatedAt, Model: hdr.Model,
			Title: hdr.Title, Bytes: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) LoadHeader(id string) (*Header, int, error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var hdr *Header
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, 0, err
		}
		if e.Type != "header" || e.Header == nil {
			continue
		}
		if hdr == nil {
			hdr = e.Header
		} else {
			mergeHeader(hdr, e.Header)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if hdr == nil {
		return nil, 0, errors.New("session header missing")
	}
	return hdr, 0, nil
}

// SetTitle records a free-form title for the session by appending a partial
// header entry. Subsequent Load / LoadHeader calls merge later headers onto
// the first, so the most recent SetTitle wins. Empty title is a no-op.
//
// We append rather than rewrite to keep the JSONL stream append-only —
// concurrent writers and the resume path both depend on that property.
func (s *Store) SetTitle(id, title string) error {
	if title == "" {
		return errors.New("session: title cannot be empty")
	}
	h := &Header{ID: id, Title: title}
	return s.append(id, Entry{Type: "header", Header: h})
}

// Sync forces the session file to disk via fsync. Returns nil if the file
// doesn't exist yet (no writes have happened).
//
// The append() path normally relies on close-after-write semantics for
// durability, but /save lets users explicitly request "I really want this
// on disk now" before yanking the laptop lid shut.
func (s *Store) Sync(id string) error {
	p := s.path(id)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Branch creates a new session based on an existing one. The new session
// inherits the parent's model + system prompt, replays the supplied
// messages, and records `ForkedFrom` so tooling can reconstruct the
// lineage. Mirrors claude-code's /branch.
func (s *Store) Branch(id string, messages []llm.Message) (string, error) {
	hdr, _, err := s.Load(id)
	if err != nil {
		return "", err
	}
	newID := s.NewSessionID()
	newHdr := Header{
		ID:       newID,
		Provider: hdr.Provider,
		Model:    hdr.Model,
		System:   hdr.System,
		ForkedFrom: &pubsess.ForkRef{
			SessionID:    id,
			MessageCount: len(messages),
		},
	}
	if err := s.WriteHeaderFull(newHdr); err != nil {
		return "", err
	}
	for _, m := range messages {
		if err := s.AppendMessage(newID, m); err != nil {
			return "", err
		}
	}
	return newID, nil
}

// Snapshot creates a named snapshot of the current session state.
func (s *Store) Snapshot(id, name string) error {
	hdr, msgs, err := s.Load(id)
	if err != nil {
		return err
	}
	hdr.ID = id + "-snapshot-" + name
	b, err := json.MarshalIndent(struct {
		Header  *Header       `json:"header"`
		Message []llm.Message `json:"messages"`
	}{hdr, msgs}, "", "  ")
	if err != nil {
		return err
	}
	snapDir := filepath.Join(s.Dir, "snapshots")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(snapDir, id+"-"+name+".json"), b, 0o644)
}

// Export streams a session's raw JSONL to w. Output preserves the original
// header line and every message entry; suitable for piping to a file or
// `metis sessions import` on another machine.
func (s *Store) Export(id string, w io.Writer) error {
	f, err := os.Open(s.path(id))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// Import reads JSONL from r and writes a new session file. Returns the new
// session id. If preferredID is non-empty and not yet in use it becomes the
// new id; otherwise a fresh UUID is generated. The first line must be a
// header entry; subsequent lines must be message entries.
func (s *Store) Import(r io.Reader, preferredID string) (string, error) {
	newID := preferredID
	if newID == "" {
		newID = s.NewSessionID()
	}
	if _, err := os.Stat(s.path(newID)); err == nil {
		return "", fmt.Errorf("session %s already exists", newID)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	if !sc.Scan() {
		return "", errors.New("import: input is empty")
	}
	var headerEntry Entry
	if err := json.Unmarshal(sc.Bytes(), &headerEntry); err != nil {
		return "", fmt.Errorf("import: header decode: %w", err)
	}
	if headerEntry.Type != "header" || headerEntry.Header == nil {
		return "", errors.New("import: first line must be a header entry")
	}
	headerEntry.Header.ID = newID
	if err := s.append(newID, headerEntry); err != nil {
		return "", err
	}

	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return "", fmt.Errorf("import: line decode: %w", err)
		}
		switch e.Type {
		case "message":
			if e.Message == nil {
				return "", errors.New("import: message entry missing message")
			}
		case "history_replace":
			// Empty messages is a valid clear snapshot.
		default:
			return "", fmt.Errorf("import: unexpected entry type %q after header", e.Type)
		}
		if err := s.append(newID, e); err != nil {
			return "", err
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return newID, nil
}

// ListSnapshots returns available snapshots for a session.
func (s *Store) ListSnapshots(id string) ([]string, error) {
	snapDir := filepath.Join(s.Dir, "snapshots")
	ents, err := os.ReadDir(snapDir)
	if err != nil {
		return nil, nil
	}
	var out []string
	prefix := id + "-"
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") {
			out = append(out, name[len(prefix):len(name)-5])
		}
	}
	return out, nil
}
