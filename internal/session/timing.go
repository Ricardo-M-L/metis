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

// MessageMetric is the user-visible footer for one completed conversation
// turn. Unlike tool timing and cumulative cost, these values must be kept per
// turn so Desktop can restore "duration / first token / tok/s" after a session
// switch or application restart.
type MessageMetric struct {
	Turn         int       `json:"turn"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	DurationMS   int64     `json:"duration_ms"`
	TTFTMS       int64     `json:"ttft_ms,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	TokPerSec    float64   `json:"tok_per_sec,omitempty"`
}

func (s *Store) timingPath(id string) string {
	return filepath.Join(s.Dir, filepath.Base(id)+".timing.jsonl")
}

func (s *Store) messageMetricsPath(id string) string {
	return filepath.Join(s.Dir, filepath.Base(id)+".message-metrics.jsonl")
}

// AppendMessageMetric durably appends one completed turn's footer metadata.
// Turns are serialized by the runtime, but Store.mu also protects callers that
// share a Store outside the WebUI.
func (s *Store) AppendMessageMetric(id string, metric MessageMetric) error {
	if s == nil || metric.Turn <= 0 || metric.StartedAt.IsZero() || metric.CompletedAt.Before(metric.StartedAt) {
		return os.ErrInvalid
	}
	raw, err := json.Marshal(metric)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.messageMetricsPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = s.syncOpenFile(f)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// ReadMessageMetrics returns every valid persisted turn in append order.
// A malformed/torn line is ignored so diagnostic metadata can never make the
// canonical conversation unreadable.
func (s *Store) ReadMessageMetrics(id string) ([]MessageMetric, error) {
	data, err := os.ReadFile(s.messageMetricsPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []MessageMetric
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var metric MessageMetric
		if json.Unmarshal(line, &metric) == nil && metric.Turn > 0 && !metric.StartedAt.IsZero() && !metric.CompletedAt.Before(metric.StartedAt) {
			out = append(out, metric)
		}
	}
	return out, nil
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

// CostSnapshot is a session's cumulative token usage, persisted so /cost
// survives a resume (the conversation JSONL restores messages but not the
// running token tally). Mirrors what Claude Code stashes in project config.
type CostSnapshot struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CacheCreateTokens int `json:"cache_create_tokens"`
	CacheReadTokens   int `json:"cache_read_tokens"`
}

func (s *Store) costPath(id string) string {
	return filepath.Join(s.Dir, filepath.Base(id)+".cost.json")
}

// WriteCost overwrites the session's cost sidecar with the latest totals.
// Atomic (temp+rename) so a concurrent reader never sees a half-written
// file. Best-effort at the call site — cost is diagnostic.
func (s *Store) WriteCost(id string, c CostSnapshot) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := s.costPath(id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.costPath(id))
}

// ReadCost loads the session's persisted cost. ok=false (no error) when the
// session has no cost sidecar (pre-feature sessions, or none written yet).
func (s *Store) ReadCost(id string) (c CostSnapshot, ok bool, err error) {
	data, rerr := os.ReadFile(s.costPath(id))
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return CostSnapshot{}, false, nil
		}
		return CostSnapshot{}, false, rerr
	}
	if jerr := json.Unmarshal(data, &c); jerr != nil {
		return CostSnapshot{}, false, nil // corrupt → treat as absent
	}
	return c, true, nil
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
