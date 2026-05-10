package agent

// monitor.go — per-line pattern-match watcher that rides on top of the
// existing jobs.Registry. The Monitor tool (internal/tools/builtin/
// monitor.go) spawns a background command via jobs.Spawn, then registers
// a Watch on this registry pointing at the job's output file. Each new
// line is regex-matched against the user-supplied patterns; matches are
// surfaced to the agent loop as MonitorEvent values which the loop
// drains at turn boundaries (alongside jobs.Notification).
//
// Why a separate registry: jobs.Notification fires once per state
// transition (Started / Exited / Killed). Monitor needs PER-LINE events
// during the job's lifetime, with rate limiting. Sharing the channel
// would either pollute the completion-notification semantics or force
// every JobNotify consumer to do regex work. A second drain that mirrors
// the same pattern is cleaner.
//
// Pattern parity:
//   - hermes-agent's process_registry.py:_check_watch_patterns —
//     per-line regex, rate-limited (15s min interval, 3 strikes max).
//   - openclaude's TaskNotification — task_notification XML enqueued at
//     turn boundaries with priority='next'.
// Both approaches converge on "watch lines, batch matches, inject as a
// system-reminder before the next user turn."

import (
	"bufio"
	"context"
	"os"
	"regexp"
	"sync"
	"time"
)

// MonitorEvent is one fired pattern match. JobID lets the agent loop
// correlate back to the spawned process; Description is the human
// label the model attached when calling the tool ("watch dev server
// for ERROR lines"); Match is the matching line content.
//
// Persisted as `<monitor_event>` reminders in the next assistant turn —
// see internal/agent/job_notify.go style.
type MonitorEvent struct {
	JobID       string
	Description string
	Match       string
	When        time.Time
}

// monitorWatch is the per-job state: which patterns to test, where in
// the output file we've already read up to, and the rate-limit
// bookkeeping mirrors hermes-agent's WATCH_MIN_INTERVAL_SECONDS=15.
type monitorWatch struct {
	jobID       string
	outputPath  string
	description string
	patterns    []*regexp.Regexp
	offset      int64
	lastEmit    time.Time
	strikes     int  // consecutive emits within the rate window
	tripped     bool // once we've struck out, drop further matches silently
	cancel      context.CancelFunc
}

// MonitorRegistry tracks active per-line watchers. Lives on agent.Loop
// (one instance per session) so the loop's drain step can reach the
// same channel the watchers publish to.
//
// Thread safety: mu guards the watches map; the events channel is
// reader-safe by construction (Go channel semantics).
type MonitorRegistry struct {
	mu      sync.Mutex
	watches map[string]*monitorWatch
	events  chan MonitorEvent

	// Tunables — exposed as struct fields (rather than constants) so
	// tests can crank pollInterval down to 10ms without sleeping the
	// real wall clock for the rate-limit window.
	pollInterval time.Duration
	rateWindow   time.Duration
	maxStrikes   int
}

// Defaults match hermes-agent's process_registry.py constants. 1s poll
// is a good balance — fast enough that the user feels live updates,
// slow enough that 100 active monitors don't pin a CPU. 15s rate window
// + 3 strikes means a chatty log floods at most 3 entries before we
// silently mute that watch (the model doesn't want 50 ERROR lines in
// the next system reminder).
const (
	defaultMonitorPoll       = 1 * time.Second
	defaultMonitorRateWindow = 15 * time.Second
	defaultMonitorMaxStrikes = 3
)

// NewMonitorRegistry builds an empty registry. eventBuf sizes the
// internal channel; tests that fire many matches at once should pass
// >=64 to avoid a non-deterministic blocked send.
func NewMonitorRegistry(eventBuf int) *MonitorRegistry {
	if eventBuf < 1 {
		eventBuf = 64
	}
	return &MonitorRegistry{
		watches:      make(map[string]*monitorWatch),
		events:       make(chan MonitorEvent, eventBuf),
		pollInterval: defaultMonitorPoll,
		rateWindow:   defaultMonitorRateWindow,
		maxStrikes:   defaultMonitorMaxStrikes,
	}
}

// Events returns the receive-only channel agent.Loop drains. Same
// shape as jobs.Registry.Notify().
func (r *MonitorRegistry) Events() <-chan MonitorEvent {
	return r.events
}

// Watch starts tailing the given output file. Returns immediately;
// the watcher goroutine runs until the process is killed or Stop is
// called for this jobID. patterns must be pre-compiled regexes —
// the tool layer is responsible for surfacing compile errors back to
// the model so it can correct its input.
//
// Idempotent on duplicate jobID: a second Watch with the same id
// replaces the first (the previous goroutine is cancelled). This
// matches hermes-agent's "one watcher per process" rule.
func (r *MonitorRegistry) Watch(jobID, outputPath, description string, patterns []*regexp.Regexp) {
	if jobID == "" || outputPath == "" || len(patterns) == 0 {
		return
	}
	r.mu.Lock()
	if existing, ok := r.watches[jobID]; ok && existing.cancel != nil {
		existing.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &monitorWatch{
		jobID:       jobID,
		outputPath:  outputPath,
		description: description,
		patterns:    patterns,
		cancel:      cancel,
	}
	r.watches[jobID] = w
	r.mu.Unlock()
	go r.run(ctx, w)
}

// Stop cancels the watcher for jobID. Safe to call after the job
// already exited (no-op). Used by the JobStop / Monitor-shutdown path.
func (r *MonitorRegistry) Stop(jobID string) {
	r.mu.Lock()
	w, ok := r.watches[jobID]
	if ok {
		delete(r.watches, jobID)
	}
	r.mu.Unlock()
	if ok && w.cancel != nil {
		w.cancel()
	}
}

// StopAll cancels every active watcher. Called when the agent loop
// exits so we don't leak goroutines past Run() returning.
func (r *MonitorRegistry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.watches {
		if w.cancel != nil {
			w.cancel()
		}
		delete(r.watches, id)
	}
}

// run is the per-watcher goroutine. Polls the output file at
// pollInterval, scans only the bytes appended since last poll, applies
// patterns line-by-line, emits MonitorEvent on a non-blocking send.
func (r *MonitorRegistry) run(ctx context.Context, w *monitorWatch) {
	tick := time.NewTicker(r.pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.scanOnce(w)
		}
	}
}

// scanOnce reads new bytes from the output file and emits events for
// matching lines. Survives transient open errors (e.g. file briefly
// missing during process startup) by silently skipping the tick.
func (r *MonitorRegistry) scanOnce(w *monitorWatch) {
	f, err := os.Open(w.outputPath)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(w.offset, 0); err != nil {
		return
	}
	scanner := bufio.NewScanner(f)
	// Generous line cap — JSON log lines or stack traces routinely
	// exceed bufio's default 64 KiB. 1 MiB is plenty for any real log
	// without enabling unbounded memory growth on a malicious tail.
	const maxLine = 1 << 20
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxLine)
	bytesRead := w.offset
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += int64(len(line)) + 1 // +1 for newline scanner stripped
		r.maybeEmit(w, line)
	}
	w.offset = bytesRead
}

// maybeEmit applies patterns to one line and respects the rate limit.
// A match within rateWindow of the previous match counts as a "strike";
// after maxStrikes consecutive strikes the watch goes silent (logged
// to debug log via the agent loop's drain side, not here).
func (r *MonitorRegistry) maybeEmit(w *monitorWatch, line string) {
	if w.tripped {
		return
	}
	matched := false
	for _, p := range w.patterns {
		if p.MatchString(line) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	now := time.Now()
	if !w.lastEmit.IsZero() && now.Sub(w.lastEmit) < r.rateWindow {
		w.strikes++
		if w.strikes > r.maxStrikes {
			w.tripped = true
			return
		}
	} else {
		// Outside the rate window — the burst counter resets so a
		// quiet hour followed by a single match doesn't carry over
		// stale strikes.
		w.strikes = 0
	}
	w.lastEmit = now

	// Non-blocking send: if the agent loop's drain hasn't run, the
	// event is dropped silently. The buffered channel (cap 64) gives
	// us a generous burst tolerance; beyond that we'd rather drop
	// monitor noise than block the watcher goroutine and let the
	// output file grow unbounded.
	select {
	case r.events <- MonitorEvent{
		JobID:       w.jobID,
		Description: w.description,
		Match:       line,
		When:        now,
	}:
	default:
	}
}
