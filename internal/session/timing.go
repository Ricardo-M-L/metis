package session

// timing.go — per-step duration sidecar for a session. The conversation
// JSONL (<id>.jsonl) stores messages; this stores "step N took X ms" in a
// parallel <id>.timing.jsonl so `metis sessions timing <id>` can show where
// a past session actually spent its wall-clock time (which Claude Code only
// exposes via OTel — metis persists it locally and retroactively browsable).
//
// A sidecar (not a new entry type in the message JSONL) keeps the
// conversation file clean and lets old sessions stay readable; a session
// with no sidecar simply has no recorded timing.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TimingStep is one recorded tool/step duration.
type TimingStep struct {
	TS        time.Time `json:"ts"`
	Tool      string    `json:"tool"`
	ElapsedMS int64     `json:"elapsed_ms"`
	IsError   bool      `json:"is_error,omitempty"`
}

func (s *Store) timingPath(id string) string {
	return filepath.Join(s.Dir, filepath.Base(id)+".timing.jsonl")
}

// TimingRecorder appends per-step timing to a session's sidecar. The agent
// runs tools in parallel, so Record is mutex-guarded. A nil *TimingRecorder
// is a valid no-op recorder, so callers (and the Loop's TimingSink) needn't
// nil-check.
type TimingRecorder struct {
	mu   sync.Mutex
	path string
}

// NewTimingRecorder returns a recorder writing this session's sidecar.
func (s *Store) NewTimingRecorder(id string) *TimingRecorder {
	return &TimingRecorder{path: s.timingPath(id)}
}

// Record appends one step. Best-effort: a write failure is dropped (timing
// is diagnostic, never load-bearing — it must not break a turn).
func (r *TimingRecorder) Record(tool string, elapsed time.Duration, isError bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(TimingStep{
		TS:        time.Now().UTC(),
		Tool:      tool,
		ElapsedMS: elapsed.Milliseconds(),
		IsError:   isError,
	})
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// ReadTiming loads a session's recorded steps in order. Returns nil (no
// error) when the session has no timing sidecar.
func (s *Store) ReadTiming(id string) ([]TimingStep, error) {
	data, err := os.ReadFile(s.timingPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TimingStep
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var st TimingStep
		if json.Unmarshal(line, &st) == nil {
			out = append(out, st)
		}
	}
	return out, nil
}
