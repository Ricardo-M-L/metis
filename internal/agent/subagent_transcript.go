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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	pubsess "github.com/Ricardo-M-L/metis/pkg/session"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// LoadSubAgentHeader reads only the immutable first JSONL record. Desktop
// uses it to check parent-session ownership without scanning a growing child
// transcript on every live detail request.
func LoadSubAgentHeader(sessionDir, agentID string) (pubsess.Header, error) {
	if sessionDir == "" || agentID == "" {
		return pubsess.Header{}, fmt.Errorf("subagent transcript: sessionDir + agentID both required")
	}
	path := filepath.Join(sessionDir, SubAgentTranscriptDirname, agentID+".jsonl")
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

// SubAgentSnapshot is the resume payload — Header + decoded message
// history. Returned by LoadSubAgentSnapshot; callers (Agent.Execute's
// resume branch) seed a fresh Loop with these Messages.
type SubAgentSnapshot struct {
	Header   pubsess.Header
	Messages []llm.Message
	Terminal *SubAgentTerminal
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
	Type     string            `json:"type"` // "header" | "message" | "terminal"
	Header   *pubsess.Header   `json:"header,omitempty"`
	Message  *llm.Message      `json:"message,omitempty"`
	Terminal *SubAgentTerminal `json:"terminal,omitempty"`
}

// SubAgentTranscript is the per-sub-agent writer. Owns an open file
// handle so AppendMessage is one syscall + flush. The Agent tool
// constructs one per spawned sub-agent (foreground or background),
// closes it on exit. nil-safe: a nil receiver means "persistence
// disabled" — Append / Close are no-ops so the foreground unit-test
// path doesn't need a writable filesystem.
type SubAgentTranscript struct {
	path string
	f    *os.File
}

// NewSubAgentTranscript opens (or creates) a sub-agent's JSONL under
// dir/SubAgentTranscriptDirname. Writes the Header as the first line.
// On any I/O error the writer returns nil + the error — callers
// should treat that as "persistence disabled" and continue without
// transcript (no fatal).
func NewSubAgentTranscript(sessionDir, agentID string, hdr pubsess.Header) (*SubAgentTranscript, error) {
	if sessionDir == "" || agentID == "" {
		return nil, fmt.Errorf("subagent transcript: sessionDir + agentID both required")
	}
	dir := filepath.Join(sessionDir, SubAgentTranscriptDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("subagent transcript mkdir: %w", err)
	}
	path := filepath.Join(dir, agentID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("subagent transcript open %s: %w", path, err)
	}
	t := &SubAgentTranscript{path: path, f: f}
	if err := t.writeEntry(subagentEntry{Type: "header", Header: &hdr}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return t, nil
}

// AppendMessage writes one llm.Message to the transcript. Safe to
// call on a nil receiver (no-op). Failures are returned but should
// be treated as advisory by the caller — the in-memory run continues
// regardless.
func (t *SubAgentTranscript) AppendMessage(m llm.Message) error {
	if t == nil || t.f == nil {
		return nil
	}
	return t.writeEntry(subagentEntry{Type: "message", Message: &m})
}

// AppendTerminal is called after the child has finished and its private
// resources have been joined. It makes the status and final output available
// to Desktop after a process restart or roster eviction.
func (t *SubAgentTranscript) AppendTerminal(terminal SubAgentTerminal) error {
	if t == nil || t.f == nil {
		return nil
	}
	return t.writeEntry(subagentEntry{Type: "terminal", Terminal: &terminal})
}

// Close flushes and closes the underlying file. Safe to call on nil.
func (t *SubAgentTranscript) Close() error {
	if t == nil || t.f == nil {
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
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal subagent entry: %w", err)
	}
	b = append(b, '\n')
	if _, err := t.f.Write(b); err != nil {
		return fmt.Errorf("write subagent entry: %w", err)
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
	if sessionDir == "" || agentID == "" {
		return nil, fmt.Errorf("subagent transcript: sessionDir + agentID both required")
	}
	path := filepath.Join(sessionDir, SubAgentTranscriptDirname, agentID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read subagent transcript %s: %w", path, err)
	}
	var snap SubAgentSnapshot
	headerSeen := false
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e subagentEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("subagent transcript line %d: %w", i+1, err)
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
		default:
			return nil, fmt.Errorf("subagent transcript line %d: unknown entry type %q", i+1, e.Type)
		}
	}
	if !headerSeen {
		return nil, fmt.Errorf("subagent transcript %s has no header", path)
	}
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
