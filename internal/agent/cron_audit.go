package agent

// cron_audit.go — silent-fire transcripts land under
// <root>/audit/<job-id>/<rfc3339>.jsonl. The user inspects them via
// `/cron audit <id> [N]` (cmd_cron_audit.go) or directly off disk for
// scripted post-processing.
//
// One file per fire (not one rolling log) so a noisy job that fires
// every minute doesn't make grep slow: the latest 100 fires for a
// hot job are 100 small files the OS can stat in ~2ms, vs scanning a
// 100MB tail of a single log. Files are JSONL so each line is a
// self-contained record — appendable mid-fire, parseable line-by-
// line with `jq`.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AuditEntry is one line in a silent-fire audit log. Mirrors the
// agent.Event surface: text deltas, tool calls, tool results, info
// messages, and the final loop-done outcome. Wall-clock timestamps so
// a post-hoc reader can reconstruct the fire's latency profile.
type AuditEntry struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // "start" | "text" | "tool_start" | "tool_result" | "info" | "loop_done" | "error"
	Text    string    `json:"text,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
}

// AuditWriter appends entries to a silent fire's JSONL transcript.
// Construct one per fire via OpenAuditLog; call Close at the end.
//
// Errors from individual Append calls are swallowed (audit is best-
// effort — a full disk shouldn't kill the cron's main work) and
// surfaced only from Close so callers that DO care can log the
// final result.
type AuditWriter struct {
	f   *os.File
	enc *json.Encoder
	err error
}

// OpenAuditLog creates (and ensures the parent dir of) a fresh audit
// file for one silent fire. `root` is the CronService.root; jobID is
// the CronJob.ID. The returned file path is exposed via Path() for
// the status-bar badge to link.
func OpenAuditLog(root, jobID string) (*AuditWriter, error) {
	if root == "" || jobID == "" {
		return nil, fmt.Errorf("audit: root and jobID required")
	}
	dir := filepath.Join(root, "audit", jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	// File name format `2026-05-13T01-23-45.jsonl` — colons replaced
	// with dashes so Windows / FAT32 are happy. Filesystem-sortable.
	name := strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339), ":", "-") + ".jsonl"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &AuditWriter{f: f, enc: json.NewEncoder(f)}, nil
}

// Path returns the on-disk path of the audit log. Useful for
// "fired silently — see <path>" notifications.
func (w *AuditWriter) Path() string {
	if w == nil || w.f == nil {
		return ""
	}
	return w.f.Name()
}

// Append records one entry. Stamps At=now if zero (caller convenience).
// Swallowed errors surface from Close.
func (w *AuditWriter) Append(e AuditEntry) {
	if w == nil || w.enc == nil || w.err != nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if err := w.enc.Encode(&e); err != nil {
		w.err = err
	}
}

// Close flushes the encoder + closes the file. Returns the first error
// hit during Append OR the close error, whichever fired first.
func (w *AuditWriter) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	if closeErr := w.f.Close(); w.err == nil {
		w.err = closeErr
	}
	return w.err
}

// AuditPath returns the directory holding silent-fire transcripts for
// a given job. `(empty, false)` when no fires have been recorded.
// Used by `/cron audit <id>` to surface the latest fire's contents.
func (s *CronService) AuditPath(jobID string) (string, bool) {
	if s == nil || jobID == "" {
		return "", false
	}
	dir := filepath.Join(s.root, "audit", jobID)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", false
	}
	return dir, true
}

// ListAuditFires returns the audit log filenames for a job, newest
// first. Empty slice + nil error means no fires yet (the audit dir
// either doesn't exist or holds zero files).
//
// The names are filesystem-sortable (RFC3339-with-dashes); we reverse
// the directory order rather than re-stating mtimes.
func (s *CronService) ListAuditFires(jobID string) ([]string, error) {
	dir, ok := s.AuditPath(jobID)
	if !ok {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}
