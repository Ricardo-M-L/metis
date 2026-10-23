package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SnapshotProgress contains aggregate diagnostics only: never file names,
// file contents, shell arguments, or credential material. Callbacks run on the
// caller goroutine and must not reenter Manager methods.
type SnapshotProgress struct {
	Stage          string        `json:"stage"`
	Elapsed        time.Duration `json:"elapsed"`
	Files          int           `json:"files"`
	Directories    int           `json:"directories"`
	Bytes          int64         `json:"bytes"`
	ProcessedFiles int           `json:"processed_files"`
	CopiedFiles    int           `json:"copied_files"`
	UnchangedFiles int           `json:"unchanged_files"`
	RemovedFiles   int           `json:"removed_files"`
}

type SnapshotStats struct {
	SnapshotProgress
	StageDurations map[string]time.Duration `json:"stage_durations"`
}

// SnapshotError preserves errors.Is/errors.As while adding the failing phase
// and scan size. The printable diagnostic deliberately excludes paths; the
// wrapped cause remains available to programmatic callers.
type SnapshotError struct {
	Stage string
	Stats SnapshotStats
	Err   error
}

func (e *SnapshotError) Error() string {
	reason := "operation failed"
	if errors.Is(e.Err, context.DeadlineExceeded) {
		reason = context.DeadlineExceeded.Error()
	} else if errors.Is(e.Err, context.Canceled) {
		reason = context.Canceled.Error()
	}
	return fmt.Sprintf("checkpoint %s: %s (elapsed %s; files %d; bytes %d; processed %d)", e.Stage, reason,
		e.Stats.Elapsed.Round(time.Millisecond), e.Stats.Files, e.Stats.Bytes, e.Stats.ProcessedFiles)
}

func (e *SnapshotError) Unwrap() error { return e.Err }

type progressContextKey struct{}

// WithProgress enables bounded progress reporting for SnapContext,
// BaselineContext and ChangedPathsContext without changing their contracts.
// Stage transitions are immediate; updates within a stage are at most 1/s.
func WithProgress(ctx context.Context, callback func(SnapshotProgress)) context.Context {
	return context.WithValue(ctx, progressContextKey{}, callback)
}

// HasBaseline is deliberately lock-free: callers use it to choose a bounded
// cold-start budget before waiting for the manager's cancellable mutex.
func (m *Manager) HasBaseline() bool { return m.hasBaseline.Load() }

func (m *Manager) LastSnapshotStats() SnapshotStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copySnapshotStats(m.lastScan)
}

func copySnapshotStats(stats SnapshotStats) SnapshotStats {
	copy := stats
	copy.StageDurations = make(map[string]time.Duration, len(stats.StageDurations))
	for stage, elapsed := range stats.StageDurations {
		copy.StageDurations[stage] = elapsed
	}
	return copy
}

type scanObserver struct {
	stats    SnapshotStats
	started  time.Time
	stageAt  time.Time
	notified time.Time
	callback func(SnapshotProgress)
}

func newScanObserver(ctx context.Context) *scanObserver {
	callback, _ := ctx.Value(progressContextKey{}).(func(SnapshotProgress))
	return &scanObserver{started: time.Now(), callback: callback, stats: SnapshotStats{
		StageDurations: make(map[string]time.Duration),
	}}
}

func (s *scanObserver) stage(name string) {
	now := time.Now()
	if s.stats.Stage != "" {
		s.stats.StageDurations[s.stats.Stage] += now.Sub(s.stageAt)
	}
	s.stats.Stage = name
	s.stageAt = now
	s.notify(true)
}

func (s *scanObserver) notify(force bool) {
	if !force && time.Since(s.notified) < time.Second {
		return
	}
	s.stats.Elapsed = time.Since(s.started)
	s.notified = time.Now()
	if s.callback != nil {
		s.callback(s.stats.SnapshotProgress)
	}
}

func (s *scanObserver) finish(err error) (SnapshotStats, error) {
	now := time.Now()
	if s.stats.Stage != "" {
		s.stats.StageDurations[s.stats.Stage] += now.Sub(s.stageAt)
	}
	s.stats.Elapsed = now.Sub(s.started)
	stats := copySnapshotStats(s.stats)
	if err != nil {
		return stats, &SnapshotError{Stage: stats.Stage, Stats: stats, Err: err}
	}
	return stats, nil
}
