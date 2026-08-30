// Package session persists conversations to JSONL on disk so they can
// be resumed or audited. One file per session under cfg.Session.Dir.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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

const (
	SystemPromptKindDefault = pubsess.SystemPromptKindDefault
	SystemPromptKindCustom  = pubsess.SystemPromptKindCustom
)

// FeedbackEntry is a log-only human remark (DSH command-feedback parity):
// it is appended to the session JSONL under type "feedback", never enters
// model context, and Load ignores it (no history effect). Kinds: "remark"
// (free text), "rating" (up/down on a specific message), "note" (message
// annotation).
type FeedbackEntry struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Rating is "up" or "down" (kind=rating only) — DSH message-feedback
	// parity: a per-message 👍/👎 signal aggregated by FeedbackStats.
	Rating string `json:"rating,omitempty"`
	// MsgIdx identifies the rated/annotated message (transcript index or
	// UI row index, depending on the surface that recorded it).
	MsgIdx string `json:"msg_idx,omitempty"`
	At     string `json:"at"` // RFC3339 timestamp
}

type Entry struct {
	Type     string         `json:"type"` // "header" | "message" | "history_replace" | "feedback"
	Header   *Header        `json:"header,omitempty"`
	Message  *llm.Message   `json:"message,omitempty"`
	Messages []llm.Message  `json:"messages,omitempty"`
	Feedback *FeedbackEntry `json:"feedback,omitempty"`
}

// AppendFeedback records a log-only human feedback entry on the session.
// Safe to call while a turn is in flight — append-only, same write path
// as messages.
func (s *Store) AppendFeedback(id, kind, text string) error {
	return s.append(id, Entry{
		Type: "feedback",
		Feedback: &FeedbackEntry{
			Kind: kind,
			Text: text,
			At:   time.Now().Format(time.RFC3339),
		},
	})
}

// AppendRating records a message-level 👍/👎 (kind=rating). msgIdx is the
// transcript index of the rated message; rating must be "up" or "down"
// (anything else is rejected — a malformed rating must not silently land).
func (s *Store) AppendRating(id, rating, msgIdx string) error {
	if rating != "up" && rating != "down" {
		return fmt.Errorf("rating must be up or down, got %q", rating)
	}
	return s.append(id, Entry{
		Type: "feedback",
		Feedback: &FeedbackEntry{
			Kind:   "rating",
			Rating: rating,
			MsgIdx: msgIdx,
			At:     time.Now().Format(time.RFC3339),
		},
	})
}

// FeedbackStats is the aggregated sidecar view over a session's feedback
// entries: how many 👍/👎 and free-text remarks landed.
type FeedbackStats struct {
	Up      int
	Down    int
	Remarks int
}

// FeedbackStats aggregates the feedback entries of ONE session.
func (s *Store) FeedbackStats(id string) (FeedbackStats, error) {
	var st FeedbackStats
	err := s.scanFeedback(id, func(f *FeedbackEntry) {
		switch {
		case f.Kind == "rating" && f.Rating == "up":
			st.Up++
		case f.Kind == "rating" && f.Rating == "down":
			st.Down++
		default:
			st.Remarks++
		}
	})
	return st, err
}

// FeedbackStatsAll aggregates feedback entries across EVERY session in
// the store dir (global lifecycle view — DSH message-feedback parity).
func (s *Store) FeedbackStatsAll() (FeedbackStats, error) {
	var st FeedbackStats
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return st, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		_ = s.scanFeedback(e.Name()[:len(e.Name())-len(".jsonl")], func(f *FeedbackEntry) {
			switch {
			case f.Kind == "rating" && f.Rating == "up":
				st.Up++
			case f.Kind == "rating" && f.Rating == "down":
				st.Down++
			default:
				st.Remarks++
			}
		})
	}
	return st, nil
}

// scanFeedback streams the feedback entries of one session without
// materializing the message history.
func (s *Store) scanFeedback(id string, fn func(*FeedbackEntry)) error {
	f, err := os.Open(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Type != "feedback" || e.Feedback == nil {
			continue
		}
		fn(e.Feedback)
	}
	return sc.Err()
}

type Store struct {
	Dir string

	// mu serializes repair + append sequences for one Store. A normal append is
	// one O_APPEND write, but recovering an unterminated trailing JSON record
	// must inspect and possibly truncate the file before that write. Without a
	// lock, another goroutine using the same Store could append between those
	// operations and have its valid record truncated.
	mu sync.Mutex

	// syncFile is an injectable fsync boundary used by durability tests. Nil
	// means (*os.File).Sync. It is deliberately private so production callers
	// cannot weaken checkpoint durability.
	syncFile func(*os.File) error

	// syncTelemetryDir is an injectable post-rename directory durability seam.
	// Nil uses the platform default. It only applies to telemetry sidecars; the
	// canonical conversation checkpoint keeps its existing durability path.
	syncTelemetryDir func(string) error
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.replaceHistoryAndMarkLocked(id, history, cursor, false); err != nil {
		return fmt.Errorf("replace history: %w", err)
	}
	return nil
}

func (s *Store) append(id string, e Entry) error {
	// 2026-05-22: switched from json.Encoder to manual Marshal +
	// single Write to guarantee atomic-per-line append. json.Encoder
	// usually writes once-per-Encode but it's not contractually
	// guaranteed; for safety we marshal in memory first so the
	// underlying file.Write is a single syscall. Regular-file writes can still
	// be short or torn on a crash, so appendEntryLocked also repairs an invalid
	// unterminated suffix before every later append.
	//
	// Pairs with Load's tolerant trailing-line handling: even if a
	// torn write does happen, the resume path now drops the bad
	// trailing line instead of aborting the whole session.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendEntryLocked(id, e, false)
}

// appendEntryLocked writes one JSONL entry. s.mu must be held. When durable is
// true the entry is fsynced before success is reported; compaction uses this
// for the history_replace commit record.
func (s *Store) appendEntryLocked(id string, e Entry, durable bool) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(s.path(id), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}

	needsSeparator, err := repairUnterminatedTail(f)
	if err == nil && needsSeparator {
		b = append([]byte{'\n'}, b...)
	}
	writeStart := int64(0)
	writeStarted := false
	if err == nil {
		var st os.FileInfo
		st, err = f.Stat()
		if err == nil {
			writeStart = st.Size()
		}
	}
	if err == nil {
		var n int
		writeStarted = true
		n, err = f.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
	}
	if err == nil && durable {
		err = s.syncOpenFile(f)
	}
	if err != nil && writeStarted {
		// Keep a failed/short append from becoming tomorrow's corrupted tail.
		// For a durable replacement this also restores the already-fsynced raw
		// checkpoint state before the error is returned to CompactNow.
		if truncateErr := f.Truncate(writeStart); truncateErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback failed session append: %w", truncateErr))
		} else if durable {
			// The raw checkpoint crossed its fsync barrier before this durable
			// replacement began. Persist the truncation too, otherwise a power
			// loss could resurrect the replacement whose fsync just failed.
			if rollbackSyncErr := s.syncOpenFile(f); rollbackSyncErr != nil {
				err = errors.Join(err, fmt.Errorf("sync rolled-back session append: %w", rollbackSyncErr))
			}
		}
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	// append historically treated a completed Write as success and ignored a
	// later Close error. Preserve that cursor contract; durable writes already
	// crossed fsync above. Returning a Close error here would make callers retry
	// an entry that is already present.
	_ = closeErr
	return nil
}

const maxTrailingRepairBytes int64 = 16 * 1024 * 1024

// repairUnterminatedTail makes the tolerant Load behaviour safe across a
// subsequent append. A short/torn O_APPEND write leaves an invalid JSON prefix
// without its terminating newline. If another record were appended directly,
// both records would become one permanently invalid line. Before appending we
// therefore discard only that unterminated invalid suffix. A valid final JSON
// record that merely lacks a newline is preserved and the caller inserts the
// missing separator.
//
// The bounded scan avoids allocating an attacker-sized malformed suffix. A
// suffix beyond the bound is rejected rather than silently discarded.
func repairUnterminatedTail(f *os.File) (needsSeparator bool, err error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false, err
	}
	size := st.Size()
	last := []byte{0}
	if _, err := f.ReadAt(last, size-1); err != nil {
		return false, err
	}
	if last[0] == '\n' {
		return false, nil
	}

	searchStart := size - maxTrailingRepairBytes
	if searchStart < 0 {
		searchStart = 0
	}
	tail := make([]byte, size-searchStart)
	if _, err := f.ReadAt(tail, searchStart); err != nil {
		return false, err
	}
	lineOffset := bytes.LastIndexByte(tail, '\n') + 1
	if lineOffset == 0 && searchStart > 0 {
		return false, fmt.Errorf("session trailing record exceeds %s repair limit", fmtBytes(maxTrailingRepairBytes))
	}
	lineStart := searchStart + int64(lineOffset)
	line := tail[lineOffset:]
	if json.Valid(line) {
		return true, nil
	}
	if lineStart == 0 {
		return false, errors.New("session contains no complete JSONL record before corrupted tail")
	}
	if err := f.Truncate(lineStart); err != nil {
		return false, fmt.Errorf("truncate corrupted session tail: %w", err)
	}
	return false, nil
}

func (s *Store) syncOpenFile(f *os.File) error {
	if s.syncFile != nil {
		return s.syncFile(f)
	}
	return f.Sync()
}

// syncLocked fsyncs an existing session file. s.mu must be held.
func (s *Store) syncLocked(id string) error {
	f, err := os.OpenFile(s.path(id), os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	err = s.syncOpenFile(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	return nil
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
	sc.Buffer(make([]byte, 1<<20), resumeScannerMaxBytes())
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
	if src.SystemPromptKind != "" {
		dst.SystemPromptKind = src.SystemPromptKind
	}
	if src.WorkDir != "" {
		dst.WorkDir = src.WorkDir
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
		// A permission-state header is a complete mode snapshot. Assigning the
		// companion field even when empty is the append-only tombstone that
		// clears a previous plan lineage after ExitPlanMode.
		dst.PrePlanMode = src.PrePlanMode
	}
	if src.Effort != "" {
		dst.Effort = src.Effort
	}
	if src.Preset != "" {
		dst.Preset = src.Preset
	}
	if src.Status != "" {
		dst.Status = src.Status
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

// ResumeListOptions limits the user-facing resume catalog. Administrative
// callers such as `metis ps` should keep using List so a live header-only
// process remains observable even before its first prompt.
type ResumeListOptions struct {
	Limit   int
	WorkDir string
}

// ListResumable returns top-level sessions that contain conversation history.
// Header-only startup artifacts and sub-agent transcripts are intentionally
// absent from the human resume picker. When WorkDir is set, sessions from a
// different project are hidden; legacy headers without a work_dir remain
// visible so old conversations are not stranded.
func (s *Store) ListResumable(opts ResumeListOptions) ([]ListEntry, error) {
	entries, err := s.List(0)
	if err != nil {
		return nil, err
	}
	out := make([]ListEntry, 0, min(len(entries), opts.Limit))
	for _, entry := range entries {
		if entry.MessageCount == 0 {
			continue
		}
		hdr, _, err := s.LoadHeader(entry.ID)
		if err != nil || hdr.SubAgentOf != "" {
			continue
		}
		if opts.WorkDir != "" && hdr.WorkDir != "" && !sameWorkDir(hdr.WorkDir, opts.WorkDir) {
			continue
		}
		out = append(out, entry)
		if opts.Limit > 0 && len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

func sameWorkDir(left, right string) bool {
	if samePath(left, right) {
		return true
	}
	leftRoot := nearestGitRoot(left)
	rightRoot := nearestGitRoot(right)
	if leftRoot != "" && rightRoot != "" {
		return samePath(leftRoot, rightRoot)
	}
	return false
}

func nearestGitRoot(path string) string {
	path, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}
	for {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func samePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo) {
		return true
	}
	if abs, err := filepath.Abs(left); err == nil {
		left = abs
	}
	if abs, err := filepath.Abs(right); err == nil {
		right = abs
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

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
		hdr, messageCount, err := s.LoadHeader(id)
		if err != nil {
			continue
		}
		out = append(out, ListEntry{
			ID: id, CreatedAt: hdr.CreatedAt, UpdatedAt: info.ModTime(), Model: hdr.Model,
			Title: hdr.Title, Bytes: info.Size(), MessageCount: messageCount,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
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
	sc.Buffer(make([]byte, 1<<20), resumeScannerMaxBytes())
	var hdr *Header
	messageCount := 0
	firstUserPrompt := ""
	lineNum := 0
	var pendingBadLine string
	for sc.Scan() {
		lineNum++
		if pendingBadLine != "" {
			return nil, 0, fmt.Errorf("decode session entry at line %d: %s", lineNum-1, pendingBadLine)
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// Match Load's recovery contract: defer a decode error until we know
			// whether another line follows. A bad final partial write is ignored;
			// corruption in the middle of the ledger remains a hard error.
			pendingBadLine = err.Error()
			continue
		}
		switch e.Type {
		case "header":
			if e.Header == nil {
				continue
			}
			if hdr == nil {
				hdr = e.Header
			} else {
				mergeHeader(hdr, e.Header)
			}
		case "message":
			if e.Message != nil {
				messageCount++
				if firstUserPrompt == "" && e.Message.Role == llm.RoleUser {
					firstUserPrompt = sessionMessageTitle(*e.Message)
				}
			}
		case "history_replace":
			messageCount = len(e.Messages)
			firstUserPrompt = ""
			for _, message := range e.Messages {
				if message.Role == llm.RoleUser {
					firstUserPrompt = sessionMessageTitle(message)
					if firstUserPrompt != "" {
						break
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if hdr == nil {
		return nil, 0, errors.New("session header missing")
	}
	if hdr.Title == "" {
		hdr.Title = firstUserPrompt
	}
	return hdr, messageCount, nil
}

func sessionMessageTitle(message llm.Message) string {
	for _, block := range message.Content {
		if block.Type != "text" {
			continue
		}
		text := strings.Join(strings.Fields(block.Text), " ")
		if text == "" {
			continue
		}
		const maxRunes = 72
		runes := []rune(text)
		if len(runes) > maxRunes {
			return string(runes[:maxRunes-1]) + "…"
		}
		return text
	}
	return ""
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncLocked(id)
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
		ID:               newID,
		Provider:         hdr.Provider,
		Model:            hdr.Model,
		System:           hdr.System,
		SystemPromptKind: hdr.SystemPromptKind,
		Effort:           hdr.Effort,
		Preset:           hdr.Preset,
		Status:           "idle",
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
	sc.Buffer(make([]byte, 1<<20), resumeScannerMaxBytes())
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
