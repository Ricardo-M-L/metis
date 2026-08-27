package session

import (
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"
)

func TestTimingRecorder_RoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewSessionID()
	rec := store.NewTimingRecorder(id)
	rec.Record("Read", 12*time.Millisecond, false)
	rec.Record("Bash", 1500*time.Millisecond, false)
	rec.Record("Edit", 30*time.Millisecond, true)

	steps, err := store.ReadTiming(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(steps))
	}
	if steps[0].Tool != "Read" || steps[0].ElapsedMS != 12 {
		t.Errorf("step0 = %+v", steps[0])
	}
	if steps[1].Tool != "Bash" || steps[1].ElapsedMS != 1500 {
		t.Errorf("step1 = %+v", steps[1])
	}
	if !steps[2].IsError {
		t.Errorf("step2 should be flagged is_error: %+v", steps[2])
	}
	if steps[2].TS.IsZero() {
		t.Error("step should carry a timestamp")
	}
}

func TestReadTiming_NoSidecarIsEmpty(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	steps, err := store.ReadTiming("never-ran")
	if err != nil {
		t.Errorf("missing sidecar should be (nil,nil), got err=%v", err)
	}
	if len(steps) != 0 {
		t.Errorf("want no steps, got %d", len(steps))
	}
}

func TestNilTimingRecorder_IsNoop(t *testing.T) {
	var rec *TimingRecorder // the Loop's TimingSink may be a nil recorder's method
	rec.Record("X", time.Second, false)
	// must not panic
}

func TestMessageMetricSidecar_RoundTrip(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	started := time.Date(2026, time.August, 27, 15, 28, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	want := MessageMetric{
		Turn:              3,
		StartedAt:         started,
		CompletedAt:       started.Add(10 * time.Second),
		DurationMS:        10_000,
		TTFTMS:            2_000,
		InputTokens:       120,
		OutputTokens:      80,
		CacheCreateTokens: 20,
		CacheReadTokens:   100,
		TokPerSec:         10,
	}
	if err := store.AppendMessageMetric("message-metrics", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadMessageMetrics("message-metrics")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Turn != want.Turn || !got[0].StartedAt.Equal(want.StartedAt) || !got[0].CompletedAt.Equal(want.CompletedAt) ||
		got[0].DurationMS != want.DurationMS || got[0].TTFTMS != want.TTFTMS ||
		got[0].InputTokens != want.InputTokens || got[0].OutputTokens != want.OutputTokens ||
		got[0].CacheCreateTokens != want.CacheCreateTokens || got[0].CacheReadTokens != want.CacheReadTokens ||
		got[0].TokPerSec != want.TokPerSec {
		t.Fatalf("message metrics = %+v, want %+v", got, want)
	}
	if missing, err := store.ReadMessageMetrics("missing"); err != nil || len(missing) != 0 {
		t.Fatalf("missing metrics = %+v, %v", missing, err)
	}
}

func TestCostSidecar_RoundTrip(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	id := store.NewSessionID()

	// Missing sidecar → (zero, false, nil), so a resumed pre-feature
	// session doesn't error and just shows 0 until the next turn.
	if c, ok, err := store.ReadCost(id); err != nil || ok || c.InputTokens != 0 {
		t.Fatalf("missing cost should be (zero,false,nil); got c=%+v ok=%v err=%v", c, ok, err)
	}

	want := CostSnapshot{InputTokens: 17000, OutputTokens: 42, CacheCreateTokens: 0, CacheReadTokens: 100}
	if err := store.WriteCost(id, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.ReadCost(id)
	if err != nil || !ok {
		t.Fatalf("ReadCost after write: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}

	// Overwrite (not append) — latest totals win.
	want2 := CostSnapshot{InputTokens: 34000, OutputTokens: 84}
	_ = store.WriteCost(id, want2)
	if got, _, _ := store.ReadCost(id); got != want2 {
		t.Errorf("overwrite: got %+v want %+v", got, want2)
	}
}

func TestTelemetryAtomicWriteTreatsDirectorySyncAsBestEffort(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "directory-sync-best-effort"
	want := CostSnapshot{InputTokens: 12, OutputTokens: 3, CacheReadTokens: 8}
	var syncedDir string
	store.syncTelemetryDir = func(dir string) error {
		syncedDir = dir
		// Windows commonly rejects FlushFileBuffers for a read-only directory
		// handle. The temp file was already fsynced and renamed at this point.
		return os.ErrPermission
	}
	if err := store.WriteCost(id, want); err != nil {
		t.Fatalf("post-rename directory sync must be non-fatal: %v", err)
	}
	if syncedDir != store.Dir {
		t.Fatalf("synced directory = %q, want %q", syncedDir, store.Dir)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("renamed telemetry = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestTelemetryAtomicWriteKeepsFileSyncAndRenameFatal(t *testing.T) {
	t.Run("temporary file sync", func(t *testing.T) {
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		injectedErr := errors.New("injected temporary file sync failure")
		store.syncFile = func(*os.File) error { return injectedErr }
		directorySyncCalls := 0
		store.syncTelemetryDir = func(string) error {
			directorySyncCalls++
			return nil
		}
		if err := store.WriteCost("file-sync-fatal", CostSnapshot{InputTokens: 1}); !errors.Is(err, injectedErr) {
			t.Fatalf("write error = %v, want temporary file sync failure", err)
		}
		if directorySyncCalls != 0 {
			t.Fatalf("directory sync ran %d times before a durable rename", directorySyncCalls)
		}
		if _, err := os.Stat(store.costPath("file-sync-fatal")); !os.IsNotExist(err) {
			t.Fatalf("failed temporary sync published target: %v", err)
		}
	})

	t.Run("rename", func(t *testing.T) {
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		const id = "rename-fatal"
		if err := os.Mkdir(store.costPath(id), 0o700); err != nil {
			t.Fatal(err)
		}
		directorySyncCalls := 0
		store.syncTelemetryDir = func(string) error {
			directorySyncCalls++
			return nil
		}
		if err := store.WriteCost(id, CostSnapshot{InputTokens: 1}); err == nil {
			t.Fatal("rename failure unexpectedly succeeded")
		}
		if directorySyncCalls != 0 {
			t.Fatalf("directory sync ran %d times after failed rename", directorySyncCalls)
		}
	})
}

func TestReconcileMessageMetricIsIdempotentAndPreservesLegacyCost(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "reconcile-message-metric"
	legacy := CostSnapshot{InputTokens: 1_000, OutputTokens: 100, CacheReadTokens: 800}
	if err := store.WriteCost(id, legacy); err != nil {
		t.Fatal(err)
	}

	// The trace observer can cross the durability boundary before the HTTP
	// handler owns final timestamps.
	usage := MessageMetric{
		Turn: 2, InputTokens: 120, OutputTokens: 30,
		CacheCreateTokens: 20, CacheReadTokens: 80,
	}
	if _, err := store.ReconcileMessageMetric(id, usage); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileMessageMetric(id, usage); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, time.August, 27, 16, 0, 0, 0, time.UTC)
	finished := usage
	finished.StartedAt = started
	finished.CompletedAt = started.Add(5 * time.Second)
	finished.DurationMS = 5_000
	finished.TTFTMS = 900
	finished.TokPerSec = 12
	if _, err := store.ReconcileMessageMetric(id, finished); err != nil {
		t.Fatal(err)
	}

	metrics, err := store.ReadMessageMetrics(id)
	if err != nil || len(metrics) != 1 {
		t.Fatalf("metrics = %+v err=%v", metrics, err)
	}
	if got := metrics[0]; !got.StartedAt.Equal(started) || got.DurationMS != 5_000 ||
		got.InputTokens != 120 || got.OutputTokens != 30 || got.CacheCreateTokens != 20 || got.CacheReadTokens != 80 {
		t.Fatalf("reconciled metric = %+v", got)
	}
	wantCost := CostSnapshot{InputTokens: 1_120, OutputTokens: 130, CacheCreateTokens: 20, CacheReadTokens: 880}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != wantCost {
		t.Fatalf("cost = %+v ok=%v err=%v, want %+v", got, ok, err, wantCost)
	}
}

func TestReconcileMessageMetricRecoversLegacyMetricWithoutCostSidecar(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "legacy-metric-without-cost"
	started := time.Date(2026, time.August, 27, 16, 0, 0, 0, time.UTC)
	legacy := MessageMetric{
		Turn: 1, StartedAt: started, CompletedAt: started.Add(time.Second),
		DurationMS: 1_000, OutputTokens: 12,
	}
	if err := store.AppendMessageMetric(id, legacy); err != nil {
		t.Fatal(err)
	}
	absolute := legacy
	absolute.InputTokens = 100
	absolute.CacheCreateTokens = 10
	absolute.CacheReadTokens = 80
	if _, err := store.ReconcileMessageMetric(id, absolute); err != nil {
		t.Fatal(err)
	}
	want := CostSnapshot{InputTokens: 100, OutputTokens: 12, CacheCreateTokens: 10, CacheReadTokens: 80}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("recovered cost = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestReconcileMessageMetricRepairsLegacyTotalBelowAccountedTurn(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "legacy-low-total"
	if err := store.WriteCost(id, CostSnapshot{OutputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, time.August, 27, 16, 0, 0, 0, time.UTC)
	metric := MessageMetric{
		Turn: 1, StartedAt: started, CompletedAt: started.Add(time.Second),
		DurationMS: 1_000, OutputTokens: 20,
	}
	if err := store.AppendMessageMetric(id, metric); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileMessageMetric(id, metric); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got.OutputTokens != 20 {
		t.Fatalf("repaired legacy total = %+v ok=%v err=%v, want output=20", got, ok, err)
	}
}

func TestReconcileMessageMetricConcurrentAbsoluteTotalsNoLostOrDoubleCount(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "parallel-child-usage"
	updates := []MessageMetric{
		{Turn: 1, InputTokens: 100, OutputTokens: 10, CacheReadTokens: 80},
		{Turn: 1, InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 20, CacheReadTokens: 240},
		{Turn: 1, InputTokens: 200, OutputTokens: 20, CacheCreateTokens: 10, CacheReadTokens: 160},
		{Turn: 1, InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 20, CacheReadTokens: 240},
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(updates))
	for _, update := range updates {
		update := update
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ReconcileMessageMetric(id, update)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	metrics, err := store.ReadMessageMetrics(id)
	if err != nil || len(metrics) != 1 {
		t.Fatalf("metrics = %+v err=%v", metrics, err)
	}
	want := CostSnapshot{InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 20, CacheReadTokens: 240}
	if got := metrics[0]; got.InputTokens != 300 || got.OutputTokens != 30 || got.CacheCreateTokens != 20 || got.CacheReadTokens != 240 {
		t.Fatalf("metric usage = %+v", got)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("cost = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestReconcileMessageMetricRepairsAfterCostCommitBeforeMetricRename(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "reconcile-crash-window"
	usage := MessageMetric{
		Turn: 1, InputTokens: 90, OutputTokens: 9,
		CacheCreateTokens: 10, CacheReadTokens: 70,
	}

	// The first fsync commits cost.json; fail the second one before the metric
	// temp file can be renamed, modeling a crash in the cross-file window.
	var syncCalls int
	injectedErr := errors.New("injected metric sync failure")
	store.syncFile = func(file *os.File) error {
		syncCalls++
		if syncCalls == 2 {
			return injectedErr
		}
		return file.Sync()
	}
	if _, err := store.ReconcileMessageMetric(id, usage); !errors.Is(err, injectedErr) {
		t.Fatalf("reconcile error = %v, want injected failure", err)
	}
	want := CostSnapshot{InputTokens: 90, OutputTokens: 9, CacheCreateTokens: 10, CacheReadTokens: 70}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("ledger after interrupted reconcile = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	if metrics, err := store.ReadMessageMetrics(id); err != nil || len(metrics) != 0 {
		t.Fatalf("failed metric rename unexpectedly became visible: %+v err=%v", metrics, err)
	}

	store.syncFile = nil
	if _, err := store.ReconcileMessageMetric(id, usage); err != nil {
		t.Fatal(err)
	}
	if syncCalls != 2 {
		t.Fatalf("sync calls = %d, want cost then metric", syncCalls)
	}
	if metrics, err := store.ReadMessageMetrics(id); err != nil || len(metrics) != 1 {
		t.Fatalf("retry did not repair metric: %+v err=%v", metrics, err)
	}
	// A further replay is a no-op because cost.json already owns the absolute
	// per-turn target.
	if _, err := store.ReconcileMessageMetric(id, usage); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("retry double-counted usage: %+v ok=%v err=%v", got, ok, err)
	}
	metrics, err := store.ReadMessageMetrics(id)
	if err != nil || len(metrics) != 1 || metrics[0].InputTokens != 90 || metrics[0].OutputTokens != 9 {
		t.Fatalf("retry did not repair metric: %+v err=%v", metrics, err)
	}
}

func TestReconcileTraceUsageSnapshotBootstrapsLegacyCostWithoutDoubleCount(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "legacy-trace-bootstrap"
	legacyTotal := CostSnapshot{InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 20, CacheReadTokens: 240}
	if err := store.WriteCost(id, legacyTotal); err != nil {
		t.Fatal(err)
	}
	snapshot := map[int]CostSnapshot{
		1: {InputTokens: 100, OutputTokens: 10, CacheReadTokens: 80},
		2: {InputTokens: 200, OutputTokens: 20, CacheCreateTokens: 20, CacheReadTokens: 160},
	}
	if err := store.ReconcileTraceUsageSnapshot(id, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileTraceUsageSnapshot(id, snapshot); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != legacyTotal {
		t.Fatalf("bootstrap duplicated legacy trace: %+v ok=%v err=%v, want %+v", got, ok, err, legacyTotal)
	}
	metrics, err := store.ReadMessageMetrics(id)
	if err != nil || len(metrics) != 2 || metrics[0].Turn != 1 || metrics[1].Turn != 2 {
		t.Fatalf("bootstrap metrics = %+v err=%v", metrics, err)
	}

	// A later live absolute update charges only the positive difference.
	live := MessageMetric{Turn: 2, InputTokens: 225, OutputTokens: 25, CacheCreateTokens: 20, CacheReadTokens: 180}
	if _, err := store.ReconcileMessageMetric(id, live); err != nil {
		t.Fatal(err)
	}
	want := CostSnapshot{InputTokens: 325, OutputTokens: 35, CacheCreateTokens: 20, CacheReadTokens: 260}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("post-bootstrap live cost = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestReconcileTraceUsageSnapshotEmptyBootstrapMarksLedgerInitialized(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "legacy-empty-trace-bootstrap"
	legacy := CostSnapshot{InputTokens: 1_000, OutputTokens: 100, CacheReadTokens: 800}
	if err := store.WriteCost(id, legacy); err != nil {
		t.Fatal(err)
	}

	// First restart has no surviving trace. This still completes the legacy
	// migration: a later turn is new usage, not historical usage already folded
	// into the flat legacy total.
	if err := store.ReconcileTraceUsageSnapshot(id, nil); err != nil {
		t.Fatal(err)
	}
	initialized, ok, err := readCostStateFile(store.costPath(id))
	if err != nil || !ok || initialized.LedgerVersion != telemetryCostLedgerVersion || len(initialized.AccountedTurns) != 0 {
		t.Fatalf("empty bootstrap state = %+v ok=%v err=%v", initialized, ok, err)
	}
	newTurn := CostSnapshot{InputTokens: 120, OutputTokens: 12, CacheCreateTokens: 10, CacheReadTokens: 90}
	if err := store.ReconcileTraceUsageSnapshot(id, map[int]CostSnapshot{1: newTurn}); err != nil {
		t.Fatal(err)
	}
	// Restart replay remains idempotent after charging the first post-migration
	// turn exactly once.
	if err := store.ReconcileTraceUsageSnapshot(id, map[int]CostSnapshot{1: newTurn}); err != nil {
		t.Fatal(err)
	}
	want := CostSnapshot{InputTokens: 1_120, OutputTokens: 112, CacheCreateTokens: 10, CacheReadTokens: 890}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("post-empty-bootstrap cost = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestReconcileTraceUsageSnapshotMigratesVersionlessNonEmptyLedger(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "versionless-nonempty-ledger"
	oldTurn := CostSnapshot{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 80}
	// v0.4.34 wrote accounted_turns before ledger_version existed. A non-empty
	// map is therefore already initialized and must not enter legacy bootstrap.
	if err := writeCostStateFile(store.costPath(id), persistedCost{
		CostSnapshot:   oldTurn,
		AccountedTurns: map[string]CostSnapshot{"1": oldTurn},
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	newTurn := CostSnapshot{InputTokens: 200, OutputTokens: 20, CacheCreateTokens: 30, CacheReadTokens: 150}
	if err := store.ReconcileTraceUsageSnapshot(id, map[int]CostSnapshot{2: newTurn}); err != nil {
		t.Fatal(err)
	}
	want := CostSnapshot{InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 30, CacheReadTokens: 230}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("versionless ledger migration cost = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	state, ok, err := readCostStateFile(store.costPath(id))
	if err != nil || !ok || state.LedgerVersion != telemetryCostLedgerVersion || len(state.AccountedTurns) != 2 {
		t.Fatalf("versionless ledger migration state = %+v ok=%v err=%v", state, ok, err)
	}
}

func TestReconcileTraceUsageSnapshotWithoutLegacyCostChargesFullUsage(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	const id = "trace-bootstrap-no-cost"
	want := CostSnapshot{InputTokens: 120, OutputTokens: 12, CacheCreateTokens: 10, CacheReadTokens: 90}
	if err := store.ReconcileTraceUsageSnapshot(id, map[int]CostSnapshot{1: want}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("trace usage = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestReconcileTraceUsageSnapshotRepairsMetricFromLedgerWithoutTrace(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "ledger-only-recovery"
	want := CostSnapshot{InputTokens: 90, OutputTokens: 30, CacheCreateTokens: 10, CacheReadTokens: 70}
	if _, err := store.ReconcileMessageMetric(id, MessageMetric{
		Turn: 1, InputTokens: 90, OutputTokens: 30, CacheCreateTokens: 10, CacheReadTokens: 70,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.messageMetricsPath(id)); err != nil {
		t.Fatal(err)
	}

	if err := store.ReconcileTraceUsageSnapshot(id, nil); err != nil {
		t.Fatal(err)
	}
	metrics, err := store.ReadMessageMetrics(id)
	if err != nil || len(metrics) != 1 || metricUsage(metrics[0]) != want {
		t.Fatalf("ledger-only repair metrics = %+v err=%v, want %+v", metrics, err, want)
	}
	if got, ok, err := store.ReadCost(id); err != nil || !ok || got != want {
		t.Fatalf("ledger-only repair cost = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestLateUsageRecomputesTokPerSec(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "late-rate"
	start := time.Now().Add(-2 * time.Second)
	if _, err := store.ReconcileMessageMetric(id, MessageMetric{
		Turn: 1, StartedAt: start, CompletedAt: start.Add(2 * time.Second), DurationMS: 2_000,
		TTFTMS: 500, OutputTokens: 10, TokPerSec: 20.0 / 3.0,
	}); err != nil {
		t.Fatal(err)
	}
	merged, err := store.ReconcileMessageMetric(id, MessageMetric{Turn: 1, OutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	const want = 40.0 / 3.0 // 20 tokens / (2s total - 0.5s TTFT)
	if math.Abs(merged.TokPerSec-want) > 1e-9 {
		t.Fatalf("late usage tok/s = %.12f, want %.12f", merged.TokPerSec, want)
	}
}

func TestLateUsagePreservesMeasuredTokPerSecWithoutGenerationTiming(t *testing.T) {
	for _, tc := range []struct {
		name       string
		durationMS int64
		ttftMS     int64
	}{
		{name: "missing TTFT", durationMS: 2_000},
		{name: "TTFT reaches completion", durationMS: 2_000, ttftMS: 2_000},
		{name: "TTFT after completion", durationMS: 2_000, ttftMS: 2_500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const measured = 7.25
			merged := mergeMessageMetric(MessageMetric{
				Turn: 1, DurationMS: tc.durationMS, TTFTMS: tc.ttftMS,
				OutputTokens: 10, TokPerSec: measured,
			}, MessageMetric{Turn: 1, OutputTokens: 20})
			if math.Abs(merged.TokPerSec-measured) > 1e-12 {
				t.Fatalf("late usage tok/s = %.12f, want measured %.12f", merged.TokPerSec, measured)
			}
		})
	}
}
