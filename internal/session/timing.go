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
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	Turn              int       `json:"turn"`
	StartedAt         time.Time `json:"started_at"`
	CompletedAt       time.Time `json:"completed_at"`
	DurationMS        int64     `json:"duration_ms"`
	TTFTMS            int64     `json:"ttft_ms,omitempty"`
	InputTokens       int64     `json:"input_tokens,omitempty"`
	OutputTokens      int64     `json:"output_tokens,omitempty"`
	CacheCreateTokens int64     `json:"cache_create_tokens,omitempty"`
	CacheReadTokens   int64     `json:"cache_read_tokens,omitempty"`
	TokPerSec         float64   `json:"tok_per_sec,omitempty"`
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
	return readMessageMetricsFile(s.messageMetricsPath(id))
}

func readMessageMetricsFile(path string) ([]MessageMetric, error) {
	data, err := os.ReadFile(path)
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
		// A usage event can reach the durable trace before the HTTP turn
		// handler has finalized timestamps (notably for background agents).
		// Keep that partial turn as a token ledger; the handler reconciles the
		// timing fields later. Presentation code deliberately hides partial
		// rows until a real StartedAt/CompletedAt pair exists.
		if json.Unmarshal(line, &metric) == nil && metric.Turn > 0 &&
			(metric.CompletedAt.IsZero() || metric.StartedAt.IsZero() || !metric.CompletedAt.Before(metric.StartedAt)) {
			out = append(out, metric)
		}
	}
	return out, nil
}

// ReconcileMessageMetric upserts the absolute telemetry known for one turn.
// Token fields are monotonic maxima, not deltas: both the trace observer and
// the request handler can safely report the same provider usage without
// double-counting it. The positive difference is added to the cumulative cost
// sidecar while Store.mu serializes concurrent background children.
//
// Timestamps are optional so provider usage can cross the durability boundary
// before the top-level request completes. A later call fills in timing while
// preserving all usage already recorded for the turn.
func (s *Store) ReconcileMessageMetric(id string, incoming MessageMetric) (MessageMetric, error) {
	if s == nil || filepath.Base(id) != id || id == "" || incoming.Turn <= 0 {
		return MessageMetric{}, os.ErrInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	metrics, err := readMessageMetricsFile(s.messageMetricsPath(id))
	if err != nil {
		return MessageMetric{}, err
	}
	metrics, previous, merged := upsertMessageMetric(metrics, incoming)

	// cost.json is the transaction ledger. Commit its per-turn absolute usage
	// before rewriting the presentation sidecar. If the process stops between
	// these two atomic renames, retry sees an already-accounted target (delta=0)
	// and repairs message-metrics without charging the turn twice.
	state, costOK, err := readCostStateFile(s.costPath(id))
	if err != nil {
		return MessageMetric{}, err
	}
	if state.AccountedTurns == nil {
		state.AccountedTurns = make(map[string]CostSnapshot)
	}
	turnKey := strconv.Itoa(incoming.Turn)
	prior, accounted := state.AccountedTurns[turnKey]
	if !accounted && costOK {
		// Migration from pre-ledger releases: cost.json already includes the
		// tokens found in a persisted metric, so bootstrap that row instead of
		// adding it again. When cost.json is absent, the metric alone is not
		// proof that those tokens reached the cumulative total, so prior stays 0.
		prior = metricUsage(previous)
	}
	target := componentMaxCost(prior, metricUsage(merged))
	delta := positiveCostDelta(prior, target)
	if err := addCostChecked(&state.CostSnapshot, delta); err != nil {
		return MessageMetric{}, err
	}
	state.AccountedTurns[turnKey] = target
	state.LedgerVersion = max(state.LedgerVersion, telemetryCostLedgerVersion)
	accountedTotal, err := sumAccountedCost(state.AccountedTurns)
	if err != nil {
		return MessageMetric{}, err
	}
	// Invariant repair for partially migrated/older sidecars: the public total
	// can never be lower than the absolute turns the same file says are already
	// accounted. Without this clamp, every replay would see delta=0 while
	// readers stayed permanently low.
	state.CostSnapshot = componentMaxCost(state.CostSnapshot, accountedTotal)
	if err := writeCostStateFile(s.costPath(id), state, s.syncOpenFile, s.syncTelemetryDir); err != nil {
		return MessageMetric{}, err
	}
	if err := writeMessageMetricsFile(s.messageMetricsPath(id), metrics, s.syncOpenFile, s.syncTelemetryDir); err != nil {
		return MessageMetric{}, err
	}
	return merged, nil
}

// ReconcileTraceUsageSnapshot imports the complete token trace for one opened
// session. A pre-ledger cost.json is treated as a migration baseline: its
// historical trace turns seed AccountedTurns without being charged again.
// Once any ledger entry exists (or no legacy cost exists), missing/increased
// turns use the normal positive-delta accounting path.
func (s *Store) ReconcileTraceUsageSnapshot(id string, usageByTurn map[int]CostSnapshot) error {
	if s == nil || filepath.Base(id) != id || id == "" {
		return os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	metrics, err := readMessageMetricsFile(s.messageMetricsPath(id))
	if err != nil {
		return err
	}
	state, costOK, err := readCostStateFile(s.costPath(id))
	if err != nil {
		return err
	}
	if state.AccountedTurns == nil {
		state.AccountedTurns = make(map[string]CostSnapshot)
	}
	// Repair the crash window where cost.json was atomically committed but the
	// message-metrics rename did not complete. The ledger itself contains the
	// absolute per-turn usage, so recovery does not depend on a buffered trace
	// row having reached disk before power loss.
	for rawTurn, usage := range state.AccountedTurns {
		turn, parseErr := strconv.Atoi(rawTurn)
		if parseErr != nil || turn <= 0 {
			continue
		}
		metrics, _, _ = upsertMessageMetric(metrics, MessageMetric{
			Turn:              turn,
			InputTokens:       int64(max(0, usage.InputTokens)),
			OutputTokens:      int64(max(0, usage.OutputTokens)),
			CacheCreateTokens: int64(max(0, usage.CacheCreateTokens)),
			CacheReadTokens:   int64(max(0, usage.CacheReadTokens)),
		})
	}
	// A non-empty pre-version ledger was already initialized by v0.4.34 and is
	// compatible as-is. The explicit version is required for the empty-snapshot
	// case: omitempty otherwise makes a completed migration indistinguishable
	// from an untouched flat legacy cost file after restart.
	ledgerInitialized := state.LedgerVersion > 0 || len(state.AccountedTurns) > 0
	legacyBootstrap := costOK && !ledgerInitialized
	state.LedgerVersion = max(state.LedgerVersion, telemetryCostLedgerVersion)
	turns := make([]int, 0, len(usageByTurn))
	for turn := range usageByTurn {
		if turn > 0 {
			turns = append(turns, turn)
		}
	}
	sort.Ints(turns)
	for _, turn := range turns {
		usage := usageByTurn[turn]
		incoming := MessageMetric{
			Turn:              turn,
			InputTokens:       int64(max(0, usage.InputTokens)),
			OutputTokens:      int64(max(0, usage.OutputTokens)),
			CacheCreateTokens: int64(max(0, usage.CacheCreateTokens)),
			CacheReadTokens:   int64(max(0, usage.CacheReadTokens)),
		}
		var previous, merged MessageMetric
		metrics, previous, merged = upsertMessageMetric(metrics, incoming)
		key := strconv.Itoa(turn)
		prior, accounted := state.AccountedTurns[key]
		if legacyBootstrap && !accounted {
			// The legacy total already includes its historical trace. Seed the
			// absolute row without adding it; the invariant clamp below repairs
			// only genuinely under-reported totals.
			prior = componentMaxCost(metricUsage(previous), metricUsage(merged))
			state.AccountedTurns[key] = prior
			continue
		}
		target := componentMaxCost(prior, metricUsage(merged))
		if err := addCostChecked(&state.CostSnapshot, positiveCostDelta(prior, target)); err != nil {
			return err
		}
		state.AccountedTurns[key] = target
	}
	accountedTotal, err := sumAccountedCost(state.AccountedTurns)
	if err != nil {
		return err
	}
	state.CostSnapshot = componentMaxCost(state.CostSnapshot, accountedTotal)
	if err := writeCostStateFile(s.costPath(id), state, s.syncOpenFile, s.syncTelemetryDir); err != nil {
		return err
	}
	return writeMessageMetricsFile(s.messageMetricsPath(id), metrics, s.syncOpenFile, s.syncTelemetryDir)
}

func upsertMessageMetric(metrics []MessageMetric, incoming MessageMetric) ([]MessageMetric, MessageMetric, MessageMetric) {
	index := -1
	var previous MessageMetric
	for i := range metrics {
		if metrics[i].Turn != incoming.Turn {
			continue
		}
		if index < 0 {
			index = i
			previous = metrics[i]
			continue
		}
		previous = mergeMessageMetric(previous, metrics[i])
		metrics[i].Turn = 0
	}
	merged := mergeMessageMetric(previous, incoming)
	if index < 0 {
		return append(metrics, merged), previous, merged
	}
	metrics[index] = merged
	compacted := metrics[:0]
	for _, metric := range metrics {
		if metric.Turn > 0 {
			compacted = append(compacted, metric)
		}
	}
	return compacted, previous, merged
}

func mergeMessageMetric(current, incoming MessageMetric) MessageMetric {
	if current.Turn == 0 {
		current.Turn = incoming.Turn
	}
	if !incoming.StartedAt.IsZero() {
		current.StartedAt = incoming.StartedAt
	}
	if !incoming.CompletedAt.IsZero() {
		current.CompletedAt = incoming.CompletedAt
	}
	if incoming.DurationMS != 0 || !incoming.CompletedAt.IsZero() {
		current.DurationMS = incoming.DurationMS
	}
	if incoming.TTFTMS != 0 {
		current.TTFTMS = incoming.TTFTMS
	}
	current.InputTokens = max(current.InputTokens, incoming.InputTokens)
	current.OutputTokens = max(current.OutputTokens, incoming.OutputTokens)
	current.CacheCreateTokens = max(current.CacheCreateTokens, incoming.CacheCreateTokens)
	current.CacheReadTokens = max(current.CacheReadTokens, incoming.CacheReadTokens)
	if incoming.TokPerSec > 0 {
		current.TokPerSec = incoming.TokPerSec
	}
	// Throughput covers generation after the first token, not the complete
	// request (which includes TTFT). A positive TTFT is also our evidence that
	// this sidecar has generation timing; without it, retain any provider/handler
	// measurement instead of replacing it with a misleading whole-turn rate.
	generationMS := current.DurationMS - current.TTFTMS
	if current.TTFTMS > 0 && generationMS > 0 && current.OutputTokens > 0 {
		current.TokPerSec = float64(current.OutputTokens) / (float64(generationMS) / 1000)
	}
	return current
}

func metricUsage(metric MessageMetric) CostSnapshot {
	return CostSnapshot{
		InputTokens:       int(max(int64(0), metric.InputTokens)),
		OutputTokens:      int(max(int64(0), metric.OutputTokens)),
		CacheCreateTokens: int(max(int64(0), metric.CacheCreateTokens)),
		CacheReadTokens:   int(max(int64(0), metric.CacheReadTokens)),
	}
}

func componentMaxCost(a, b CostSnapshot) CostSnapshot {
	return CostSnapshot{
		InputTokens:       max(a.InputTokens, b.InputTokens),
		OutputTokens:      max(a.OutputTokens, b.OutputTokens),
		CacheCreateTokens: max(a.CacheCreateTokens, b.CacheCreateTokens),
		CacheReadTokens:   max(a.CacheReadTokens, b.CacheReadTokens),
	}
}

func positiveCostDelta(before, after CostSnapshot) CostSnapshot {
	return CostSnapshot{
		InputTokens:       max(0, after.InputTokens-before.InputTokens),
		OutputTokens:      max(0, after.OutputTokens-before.OutputTokens),
		CacheCreateTokens: max(0, after.CacheCreateTokens-before.CacheCreateTokens),
		CacheReadTokens:   max(0, after.CacheReadTokens-before.CacheReadTokens),
	}
}

func addCostChecked(total *CostSnapshot, delta CostSnapshot) error {
	if total == nil {
		return os.ErrInvalid
	}
	maxInt := int(^uint(0) >> 1)
	values := []struct {
		name string
		dst  *int
		add  int
	}{
		{"input tokens", &total.InputTokens, delta.InputTokens},
		{"output tokens", &total.OutputTokens, delta.OutputTokens},
		{"cache-create tokens", &total.CacheCreateTokens, delta.CacheCreateTokens},
		{"cache-read tokens", &total.CacheReadTokens, delta.CacheReadTokens},
	}
	for _, value := range values {
		if value.add < 0 || *value.dst < 0 || value.add > maxInt-*value.dst {
			return errors.New("telemetry " + value.name + " overflow")
		}
		*value.dst += value.add
	}
	return nil
}

func sumAccountedCost(turns map[string]CostSnapshot) (CostSnapshot, error) {
	var total CostSnapshot
	for _, usage := range turns {
		if err := addCostChecked(&total, usage); err != nil {
			return CostSnapshot{}, err
		}
	}
	return total, nil
}

func writeMessageMetricsFile(path string, metrics []MessageMetric, syncFile func(*os.File) error, syncDir func(string) error) error {
	var buf bytes.Buffer
	for _, metric := range metrics {
		raw, err := json.Marshal(metric)
		if err != nil {
			return err
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return atomicWriteTelemetryFile(path, buf.Bytes(), syncFile, syncDir)
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

const telemetryCostLedgerVersion = 1

// persistedCost is intentionally backward compatible with the historical
// flat CostSnapshot JSON. LedgerVersion distinguishes a completed bootstrap
// with zero surviving trace turns from a never-migrated legacy file;
// AccountedTurns is the crash-safe idempotency ledger used by Desktop's trace
// observer. Public callers still read CostSnapshot.
type persistedCost struct {
	CostSnapshot
	LedgerVersion  int                     `json:"ledger_version,omitempty"`
	AccountedTurns map[string]CostSnapshot `json:"accounted_turns,omitempty"`
}

func (s *Store) costPath(id string) string {
	return filepath.Join(s.Dir, filepath.Base(id)+".cost.json")
}

// WriteCost overwrites the session's cost sidecar with the latest totals.
// Atomic (temp+rename) so a concurrent reader never sees a half-written
// file. Best-effort at the call site — cost is diagnostic.
func (s *Store) WriteCost(id string, c CostSnapshot) error {
	if s == nil {
		return os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, _, err := readCostStateFile(s.costPath(id))
	if err != nil {
		return err
	}
	// TUI/CLI callers write an absolute session total. Never let such a write
	// erase late background usage already committed by the per-turn ledger.
	accounted, err := sumAccountedCost(state.AccountedTurns)
	if err != nil {
		return err
	}
	state.CostSnapshot = componentMaxCost(c, accounted)
	return writeCostStateFile(s.costPath(id), state, s.syncOpenFile, s.syncTelemetryDir)
}

func writeCostFile(path string, c CostSnapshot) error {
	return writeCostStateFile(path, persistedCost{CostSnapshot: c}, nil, nil)
}

func writeCostStateFile(path string, state persistedCost, syncFile func(*os.File) error, syncDir func(string) error) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWriteTelemetryFile(path, data, syncFile, syncDir)
}

// ReadCost loads the session's persisted cost. ok=false (no error) when the
// session has no cost sidecar (pre-feature sessions, or none written yet).
func (s *Store) ReadCost(id string) (c CostSnapshot, ok bool, err error) {
	if s == nil {
		return CostSnapshot{}, false, os.ErrInvalid
	}
	return readCostFile(s.costPath(id))
}

// MaxAccountedTurn returns the highest turn committed to the idempotency
// ledger. Desktop uses it as a numbering floor when the presentation metric
// and trace sidecars were lost after cost.json was already atomically renamed.
// Legacy flat cost files have no per-turn ledger and return zero.
func (s *Store) MaxAccountedTurn(id string) (int, error) {
	if s == nil || filepath.Base(id) != id || id == "" {
		return 0, os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, _, err := readCostStateFile(s.costPath(id))
	if err != nil {
		return 0, err
	}
	maxTurn := 0
	for rawTurn := range state.AccountedTurns {
		turn, err := strconv.Atoi(rawTurn)
		if err == nil && turn > maxTurn {
			maxTurn = turn
		}
	}
	return maxTurn, nil
}

func readCostFile(path string) (c CostSnapshot, ok bool, err error) {
	state, ok, err := readCostStateFile(path)
	return state.CostSnapshot, ok, err
}

func readCostStateFile(path string) (state persistedCost, ok bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return persistedCost{}, false, nil
		}
		return persistedCost{}, false, rerr
	}
	if jerr := json.Unmarshal(data, &state); jerr != nil {
		return persistedCost{}, false, nil // corrupt → treat as absent
	}
	return state, true, nil
}

func atomicWriteTelemetryFile(path string, data []byte, syncFile func(*os.File) error, syncDir func(string) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".telemetry-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if syncFile == nil {
		syncFile = (*os.File).Sync
	}
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if syncDir == nil {
		syncDir = syncTelemetryDirectory
	}
	// The durable payload is already fsynced and atomically renamed. Directory
	// syncing only strengthens rename durability across sudden power loss, and
	// some supported platforms (notably Windows) reject it even though the
	// target file is valid. Never turn that post-commit limitation into a failed
	// turn/reconciliation result.
	_ = syncDir(dir)
	return nil
}

func syncTelemetryDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return closeErr
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
