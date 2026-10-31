package agent

// subagent_transcript.go — sub-agent persistence + resume (G.4, 2026-05-12).
// Mirrors claude-code's resumeAgent.ts: transcript is written incrementally
// per turn to a JSONL file under <session-dir>/subagents/<agent_id>.jsonl,
// and `/agents resume <id>` (or the schema field `resume_from`) loads
// that file and seeds a fresh sub-loop with the recovered Messages.
//
// Layout choice — flat per-sub-agent file under a `subagents/` subdir
// next to the main sessions:
//
//   ~/.metis/sessions/
//     2026-05-12T09:00:00.jsonl       ← main agent session
//     subagents/
//       agt-d3a91b07.jsonl            ← background sub-agent
//       agt-7e6234ab.jsonl            ← named teammate "alice"
//
// Per-parent nesting (sessions/<parent>/subagents/) was considered but
// rejected: 1) Spawn-from-anywhere case (a CLI tool that doesn't have a
// session yet) can't fit into per-parent layout; 2) Resume from a
// different parent session needs to find the file regardless of which
// parent ran first; 3) flat layout matches BashList output naming so
// users get familiar geography.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pubsess "github.com/Ricardo-M-L/metis/pkg/session"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// LoadSubAgentHeader reads only the immutable first JSONL record. Desktop
// uses it to check parent-session ownership without scanning a growing child
// transcript on every live detail request.
func LoadSubAgentHeader(sessionDir, agentID string) (pubsess.Header, error) {
	path, err := subAgentTranscriptPath(sessionDir, agentID)
	if err != nil {
		return pubsess.Header{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return pubsess.Header{}, err
	}
	defer f.Close()
	line, err := bufio.NewReader(io.LimitReader(f, 64<<10)).ReadBytes('\n')
	if err != nil {
		return pubsess.Header{}, fmt.Errorf("read subagent header: %w", err)
	}
	var entry subagentEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return pubsess.Header{}, fmt.Errorf("decode subagent header: %w", err)
	}
	if entry.Type != "header" || entry.Header == nil || entry.Header.ID != agentID {
		return pubsess.Header{}, fmt.Errorf("invalid subagent header for %q", agentID)
	}
	return *entry.Header, nil
}

// SubAgentTranscriptDirname is the subdirectory under the main session
// directory where sub-agent JSONLs live. Exported so the slash command
// `/agents resume` + tests can find them deterministically.
const SubAgentTranscriptDirname = "subagents"

// ErrSubAgentTranscriptBusy means another process still owns the producer.
var ErrSubAgentTranscriptBusy = errors.New("subagent transcript is already owned by another running producer")

// SubAgentSnapshot is the resume payload — Header + decoded message
// history. Returned by LoadSubAgentSnapshot; callers (Agent.Execute's
// resume branch) seed a fresh Loop with these Messages.
type SubAgentSnapshot struct {
	Header           pubsess.Header
	Messages         []llm.Message
	Terminal         *SubAgentTerminal
	Handoff          *SubAgentHandoffBinding
	Execution        *SubAgentExecutionConfig
	PendingFollowups []SubAgentFollowup
}

// SubAgentHandoffBinding pins a durable mailbox to the transcript that owns
// its acknowledgements. Resuming that transcript cannot change its address.
type SubAgentHandoffBinding struct {
	Scope     string `json:"scope"`
	Recipient string `json:"recipient"`
}

type subagentHistoryCheckpoint struct {
	Offset   int           `json:"offset"`
	Reset    bool          `json:"reset,omitempty"`
	Messages []llm.Message `json:"messages"`
}

// SubAgentTerminal records the last known lifecycle state independently of
// the parent turn. A background child can finish after its parent tool_result
// has already been saved, so that result cannot be the source of final status.
type SubAgentTerminal struct {
	Status     string    `json:"status"`
	Background bool      `json:"background"`
	EndedAt    time.Time `json:"endedAt"`
	Output     string    `json:"output,omitempty"`
	Result     string    `json:"result,omitempty"`
	StopHint   string    `json:"stopHint,omitempty"`
	ExitError  string    `json:"exitError,omitempty"`
}

// subagentEntry mirrors the JSONL line shape used for the main
// internal/session.Entry. Kept private here so the file format
// stays opaque to callers — they only see SubAgentSnapshot.
type subagentEntry struct {
	Type        string                     `json:"type"` // header, message, terminal, checkpoint, handoff, execution
	Header      *pubsess.Header            `json:"header,omitempty"`
	Message     *llm.Message               `json:"message,omitempty"`
	Terminal    *SubAgentTerminal          `json:"terminal,omitempty"`
	Checkpoint  *subagentHistoryCheckpoint `json:"checkpoint,omitempty"`
	Handoff     *SubAgentHandoffBinding    `json:"handoff,omitempty"`
	Execution   *SubAgentExecutionConfig   `json:"execution,omitempty"`
	Followup    *SubAgentFollowup          `json:"followup,omitempty"`
	FollowupAck *subAgentFollowupAck       `json:"followup_ack,omitempty"`
}

// SubAgentTranscript serializes the per-sub-agent writer. Checkpoint owns the
// durable producer boundary; advisory AppendMessage supports legacy consumers.
// The Agent tool
// constructs one per spawned sub-agent (foreground or background),
// closes it on exit. nil-safe: a nil receiver means "persistence
// disabled" — Append / Close are no-ops so the foreground unit-test
// path doesn't need a writable filesystem.
type SubAgentTranscript struct {
	mu               sync.Mutex
	path             string
	f                *os.File
	historyDigests   [][sha256.Size]byte
	handoff          *SubAgentHandoffBinding
	execution        *SubAgentExecutionConfig
	pendingFollowups []SubAgentFollowup
	writeErr         error
	syncFile         func() error // injectable only by package tests; defaults to f.Sync
}

// NewSubAgentTranscript opens (or creates) a sub-agent's JSONL under
// dir/SubAgentTranscriptDirname. Writes the Header as the first line.
// On any I/O error the writer returns nil + the error — callers
// should treat that as "persistence disabled" and continue without
// transcript (no fatal).
func NewSubAgentTranscript(sessionDir, agentID string, hdr pubsess.Header) (*SubAgentTranscript, error) {
	return newSubAgentTranscript(sessionDir, agentID, hdr, false)
}

// NewDurableSubAgentTranscript holds an OS-backed nonblocking writer lease
// before creating history and refuses to replace an existing transcript.
// Process exit releases the writer lease automatically.
func NewDurableSubAgentTranscript(sessionDir, agentID string, hdr pubsess.Header) (*SubAgentTranscript, error) {
	return newSubAgentTranscript(sessionDir, agentID, hdr, true)
}

func newSubAgentTranscript(sessionDir, agentID string, hdr pubsess.Header, durable bool) (*SubAgentTranscript, error) {
	if sessionDir == "" || !ValidSubAgentID(agentID) || hdr.ID != agentID {
		return nil, fmt.Errorf("subagent transcript: sessionDir + matching valid agentID required")
	}
	dir := filepath.Join(sessionDir, SubAgentTranscriptDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("subagent transcript mkdir: %w", err)
	}
	path := filepath.Join(dir, agentID+".jsonl")
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if durable {
		flags = os.O_CREATE | os.O_RDWR
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("subagent transcript open %s: %w", path, err)
	}
	if durable {
		if err := trySubAgentTranscriptLock(f); err != nil {
			_ = f.Close()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if info.Size() != 0 {
			_ = f.Close()
			return nil, fmt.Errorf("durable subagent transcript already exists; resume it instead of replacing acknowledged history")
		}
	}
	t := &SubAgentTranscript{path: path, f: f}
	if err := t.writeEntry(subagentEntry{Type: "header", Header: &hdr}); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sync subagent header: %w", err)
	}
	if err := syncSubAgentDirectory(dir); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syncSubAgentDirectory(sessionDir); err != nil {
		_ = f.Close()
		return nil, err
	}
	return t, nil
}

// ResumeSubAgentTranscript preserves acknowledged history in place. The last
// incomplete JSONL record was never a checkpoint and is removed before append.
func ResumeSubAgentTranscript(sessionDir, agentID string) (*SubAgentTranscript, error) {
	path, err := subAgentTranscriptPath(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open resumed subagent transcript: %w", err)
	}
	if err := trySubAgentTranscriptLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("subagent transcript is not a regular file")
	}
	data, err := io.ReadAll(io.NewSectionReader(f, 0, info.Size()))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	snap, err := loadSubAgentSnapshotData(data, agentID, path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("subagent transcript was replaced while acquiring its producer lease")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if err := f.Truncate(int64(bytes.LastIndexByte(data, '\n') + 1)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("trim incomplete subagent checkpoint: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	digests, err := subAgentMessageDigests(snap.Messages)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &SubAgentTranscript{path: path, f: f, historyDigests: digests, handoff: snap.Handoff, execution: cloneSubAgentExecution(snap.Execution), pendingFollowups: append([]SubAgentFollowup{}, snap.PendingFollowups...)}, nil
}

func syncSubAgentDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open subagent directory for sync: %w", err)
	}
	defer f.Close()
	if err := syncSubAgentDirectoryFile(f); err != nil {
		return fmt.Errorf("sync subagent directory: %w", err)
	}
	return nil
}

// BindHandoff synchronously saves the mailbox before any receipt can be issued.
func (t *SubAgentTranscript) BindHandoff(scope, recipient string) error {
	if t == nil {
		return fmt.Errorf("durable subagent transcript unavailable")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return fmt.Errorf("subagent transcript is closed")
	}
	if scope == "" || recipient == "" {
		return fmt.Errorf("durable subagent mailbox requires scope and recipient")
	}
	if t.handoff != nil {
		if t.handoff.Scope != scope || t.handoff.Recipient != recipient {
			return fmt.Errorf("durable subagent mailbox cannot change on resume")
		}
		return t.syncLocked()
	}
	binding := &SubAgentHandoffBinding{Scope: scope, Recipient: recipient}
	if err := t.writeEntryLocked(subagentEntry{Type: "handoff", Handoff: binding}); err != nil {
		return err
	}
	if err := t.syncLocked(); err != nil {
		return err
	}
	t.handoff = binding
	return nil
}

// Checkpoint saves the exact producer-owned history before it may acknowledge
// delivery or start another provider call. Append-only suffixes use one record;
// edits and compaction replace the replayed history with one reset record.
func (t *SubAgentTranscript) Checkpoint(history []llm.Message) error {
	if t == nil {
		return fmt.Errorf("durable subagent transcript unavailable")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return fmt.Errorf("subagent transcript is closed")
	}
	if t.writeErr != nil {
		return t.writeErr
	}
	digests, err := subAgentMessageDigests(history)
	if err != nil {
		return err
	}
	offset, reset := len(t.historyDigests), len(t.historyDigests) > len(digests)
	if !reset {
		for i, digest := range t.historyDigests {
			if digest != digests[i] {
				reset = true
				break
			}
		}
	}
	if reset {
		offset = 0
	}
	if reset || offset < len(history) {
		messages := append([]llm.Message{}, history[offset:]...)
		entry := subagentEntry{Type: "checkpoint", Checkpoint: &subagentHistoryCheckpoint{Offset: offset, Reset: reset, Messages: messages}}
		if err := t.writeEntryLocked(entry); err != nil {
			return err
		}
	}
	remaining := make([]SubAgentFollowup, 0, len(t.pendingFollowups))
	for _, followup := range t.pendingFollowups {
		if !subAgentFollowupConsumed(history, followup) {
			remaining = append(remaining, followup)
			continue
		}
		if err := t.writeEntryLocked(subagentEntry{Type: "followup_ack", FollowupAck: &subAgentFollowupAck{ID: followup.ID}}); err != nil {
			return err
		}
	}
	if err := t.syncLocked(); err != nil {
		return err
	}
	t.historyDigests = digests
	t.pendingFollowups = remaining
	return nil
}

func subAgentMessageDigests(history []llm.Message) ([][sha256.Size]byte, error) {
	digests := make([][sha256.Size]byte, len(history))
	for i, m := range history {
		encoded, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("marshal subagent checkpoint message: %w", err)
		}
		digests[i] = sha256.Sum256(encoded)
	}
	return digests, nil
}

func (t *SubAgentTranscript) syncLocked() error {
	if t.f == nil {
		return fmt.Errorf("subagent transcript is closed")
	}
	if t.writeErr != nil {
		return t.writeErr
	}
	syncFile := t.f.Sync
	if t.syncFile != nil {
		syncFile = t.syncFile
	}
	if err := syncFile(); err != nil {
		t.writeErr = fmt.Errorf("sync subagent checkpoint: %w", err)
		return t.writeErr
	}
	return nil
}

// AppendMessage writes one llm.Message to the transcript. Safe to
// call on a nil receiver (no-op). Failures are returned but should
// be treated as advisory by the caller — the in-memory run continues
// regardless.
func (t *SubAgentTranscript) AppendMessage(m llm.Message) error {
	if t == nil {
		return nil
	}
	return t.writeEntry(subagentEntry{Type: "message", Message: &m})
}

// AppendTerminal is called after the child has finished and its private
// resources have been joined. It makes the status and final output available
// to Desktop after a process restart or roster eviction.
func (t *SubAgentTranscript) AppendTerminal(terminal SubAgentTerminal) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	if err := t.writeEntryLocked(subagentEntry{Type: "terminal", Terminal: &terminal}); err != nil {
		return err
	}
	return t.syncLocked()
}

// Close closes the file and releases its producer lease. Safe to call on nil.
func (t *SubAgentTranscript) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	return err
}

// Path returns the absolute path the transcript is being written to.
// Used by Agent.Execute to surface the location in error messages.
func (t *SubAgentTranscript) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

func (t *SubAgentTranscript) writeEntry(e subagentEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	return t.writeEntryLocked(e)
}

func (t *SubAgentTranscript) writeEntryLocked(e subagentEntry) error {
	if t.writeErr != nil {
		return t.writeErr
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal subagent entry: %w", err)
	}
	b = append(b, '\n')
	if n, err := t.f.Write(b); err != nil || n != len(b) {
		if err == nil {
			err = io.ErrShortWrite
		}
		t.writeErr = fmt.Errorf("write subagent entry: %w", err)
		return t.writeErr
	}
	return nil
}

// LoadSubAgentSnapshot reads a sub-agent transcript from disk and
// returns the Header + replayed Messages. The agent_id format
// includes the `agt-` prefix; pass either with or without (we trim
// when probing the file path).
//
// Returns (nil, error) if:
//   - the file doesn't exist
//   - the JSON is malformed
//   - the first entry isn't a Header (file was corrupted mid-write)
//
// On clean read returns a SubAgentSnapshot the caller can hand
// to agent.NewLoop + sub.Restore for a warm-start resume.
func LoadSubAgentSnapshot(sessionDir, agentID string) (*SubAgentSnapshot, error) {
	path, err := subAgentTranscriptPath(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read subagent transcript %s: %w", path, err)
	}
	return loadSubAgentSnapshotData(data, agentID, path)
}

func loadSubAgentSnapshotData(data []byte, agentID, path string) (*SubAgentSnapshot, error) {
	var snap SubAgentSnapshot
	var followups subAgentFollowupReplay
	headerSeen := false
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e subagentEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("subagent transcript line %d: %w", i+1, err)
		}
		if !headerSeen && (e.Type != "header" || e.Header == nil || e.Header.ID != agentID) {
			return nil, fmt.Errorf("subagent transcript line %d: invalid first header", i+1)
		}
		switch e.Type {
		case "header":
			if e.Header == nil {
				return nil, fmt.Errorf("subagent transcript line %d: header entry with nil header", i+1)
			}
			if !headerSeen {
				snap.Header = *e.Header
				headerSeen = true
			} else {
				// Subsequent headers are merge-style (e.g. /title update).
				mergeSubagentHeader(&snap.Header, e.Header)
			}
		case "message":
			if e.Message == nil {
				return nil, fmt.Errorf("subagent transcript line %d: message entry with nil message", i+1)
			}
			snap.Messages = append(snap.Messages, *e.Message)
		case "terminal":
			if e.Terminal == nil {
				return nil, fmt.Errorf("subagent transcript line %d: terminal entry with nil terminal", i+1)
			}
			terminal := *e.Terminal
			snap.Terminal = &terminal
		case "handoff":
			if !headerSeen || e.Handoff == nil || e.Handoff.Scope == "" || e.Handoff.Recipient == "" {
				return nil, fmt.Errorf("subagent transcript line %d: invalid handoff binding", i+1)
			}
			if snap.Handoff != nil && *snap.Handoff != *e.Handoff {
				return nil, fmt.Errorf("subagent transcript line %d: handoff binding changed", i+1)
			}
			binding := *e.Handoff
			snap.Handoff = &binding
		case "execution":
			config, err := readSubAgentExecution(snap.Execution, e.Execution)
			if err != nil {
				return nil, fmt.Errorf("subagent transcript line %d: %w", i+1, err)
			}
			snap.Execution = config
		case "followup":
			if err := followups.enqueue(e.Followup); err != nil {
				return nil, fmt.Errorf("subagent transcript line %d: %w", i+1, err)
			}
		case "followup_ack":
			if err := followups.acknowledge(e.FollowupAck); err != nil {
				return nil, fmt.Errorf("subagent transcript line %d: %w", i+1, err)
			}
		case "checkpoint":
			if !headerSeen || e.Checkpoint == nil || e.Checkpoint.Offset < 0 {
				return nil, fmt.Errorf("subagent transcript line %d: invalid history checkpoint", i+1)
			}
			cp := e.Checkpoint
			if cp.Reset {
				if cp.Offset != 0 {
					return nil, fmt.Errorf("subagent transcript line %d: reset checkpoint offset must be zero", i+1)
				}
				snap.Messages = nil
			}
			if cp.Offset != len(snap.Messages) {
				return nil, fmt.Errorf("subagent transcript line %d: checkpoint offset %d differs from history length %d", i+1, cp.Offset, len(snap.Messages))
			}
			snap.Messages = append(snap.Messages, cp.Messages...)
			followups.pending = pendingSubAgentFollowups(cp.Messages, followups.pending)
			snap.Terminal = nil
		default:
			return nil, fmt.Errorf("subagent transcript line %d: unknown entry type %q", i+1, e.Type)
		}
	}
	if !headerSeen {
		return nil, fmt.Errorf("subagent transcript %s has no header", path)
	}
	snap.PendingFollowups = append([]SubAgentFollowup{}, followups.pending...)
	return &snap, nil
}

// ListSubAgentTranscripts returns the agent_ids that have on-disk
// transcripts in sessionDir. Used by `/agents resume` to enumerate
// candidates. Returns empty slice (no error) when the dir doesn't
// exist yet — fresh metis install has no sub-agents.
func ListSubAgentTranscripts(sessionDir string) ([]string, error) {
	dir := filepath.Join(sessionDir, SubAgentTranscriptDirname)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".jsonl"))
	}
	return out, nil
}

// mergeSubagentHeader applies non-zero fields from src onto dst.
// Used for follow-up header writes (currently rare — we don't have
// a SetTitle equivalent for sub-agents yet, but the helper keeps
// future feature flexibility cheap).
func mergeSubagentHeader(dst, src *pubsess.Header) {
	if src == nil {
		return
	}
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.TeammateName != "" {
		dst.TeammateName = src.TeammateName
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
}

// NewSubAgentHeader is a convenience helper for callers (Agent.Execute)
// that don't want to manually fill the Header — populates ID, CreatedAt,
// Model, plus the G.4-specific SubAgentOf / TeammateName fields.
func NewSubAgentHeader(agentID, model, parentSessionID, teammateName, workDir, mode string) pubsess.Header {
	return pubsess.Header{
		ID:           agentID,
		CreatedAt:    time.Now(),
		Model:        model,
		WorkDir:      workDir,
		Mode:         mode,
		SubAgentOf:   parentSessionID,
		TeammateName: teammateName,
	}
}
