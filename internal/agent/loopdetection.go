// Package agent implements Metis's agent loop with streaming,
// tool execution, and memory integration.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// LoopDetectorKind classifies the type of loop detected.
type LoopDetectorKind string

const (
	LoopGenericRepeat        LoopDetectorKind = "generic_repeat"
	LoopPollNoProgress       LoopDetectorKind = "known_poll_no_progress"
	LoopGlobalCircuitBreaker LoopDetectorKind = "global_circuit_breaker"
	LoopPingPong             LoopDetectorKind = "ping_pong"
	// LoopSignatureRepeat is the crush-style detection: SHA-256 over
	// (toolName, JSON(input), result) for each step's tool batch, with
	// a sliding window. When the same signature appears more than
	// `SignatureMaxRepeats` times within the window the agent is
	// stuck in a real loop (same call → same result, repeated). This
	// catches the 2026-05-08 video bug where the model spent 1h 18m
	// retrying `cd … && git rebase --continue` on an unresolved
	// conflict because the prior detector kinds (counts by tool name
	// only) didn't trigger soon enough.
	LoopSignatureRepeat LoopDetectorKind = "signature_repeat"
)

// LoopDetector monitors tool call patterns and detects repetitive behavior.
// Inspired by OpenClaw's tool-loop-detection.ts and crush's
// loop_detection.go (sliding-window SHA-256 signatures).
type LoopDetector struct {
	mu sync.RWMutex

	// Thresholds for the count-based heuristics.
	WarningThreshold  int
	CriticalThreshold int
	GlobalThreshold   int

	// Sliding-window signature detection (crush parity). Window is
	// the last N steps; if any signature shows up more than MaxRepeats
	// times within the window, ShouldAbort flips to true.
	//
	// Why two parameters instead of one ratio: a 4/10 threshold lets
	// transient retries through (legitimate "fetch failed → retry once
	// → ok" patterns) but catches the dead-loop case decisively.
	SignatureWindowSize int
	SignatureMaxRepeats int

	// State
	callCounts       map[string]int // tool name -> count in current streak
	toolSeq          []string       // recent tool call sequence
	globalCount      int            // total tool calls this session
	lastProgress     time.Time      // last successful non-tool-use turn
	pollPatterns     map[string]int // detected polling patterns
	pingPongPairs    map[string]int // ping-pong detection pairs
	signatureWindow  []string       // sliding window of step signatures
	signatureTripped bool           // sticky flag once the window threshold fired

	// Callbacks
	onWarning  func(kind LoopDetectorKind, count int, msg string)
	onCritical func(kind LoopDetectorKind, count int, msg string)
}

// NewLoopDetector creates a new loop detector with default thresholds.
//
// Signature window: 10 steps / 5 repeats matches crush's defaults
// (`internal/agent/loop_detection.go:11-13`). 5 repetitions in a 10-step
// window means at least half the recent steps are byte-for-byte the
// same call+result — that's a real loop, not flaky retries.
//
// GlobalThreshold raised 2026-05-10: a real "write tests for every file
// in this dir" task routinely needs 100-200 reads + writes + edits in a
// single Run. The previous 80-call cap killed legitimate work mid-flight
// (user report image #2: "edited 22 files, 31 reads, then loop detector
// aborted: 80 total tool calls"). 250 keeps the per-tool repeat (20),
// signature-window (5×10), and ping-pong detectors as the actual loop
// guards; the global cap exists only to bound runaway sessions, not to
// limit deliberate multi-file work. Override via `[loop_detection].global`
// in config.toml if you need a tighter (or looser) bound.
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		WarningThreshold:  10,
		CriticalThreshold: 20,
		GlobalThreshold:   250,

		SignatureWindowSize: 10,
		SignatureMaxRepeats: 5,

		callCounts:      make(map[string]int),
		toolSeq:         make([]string, 0, 100),
		pollPatterns:    make(map[string]int),
		pingPongPairs:   make(map[string]int),
		signatureWindow: make([]string, 0, 10),
		lastProgress:    time.Now(),
	}
}

// Record records a tool call for loop detection.
func (d *LoopDetector) Record(toolName string, input map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.globalCount++
	d.toolSeq = append(d.toolSeq, toolName)
	if len(d.toolSeq) > 100 {
		d.toolSeq = d.toolSeq[len(d.toolSeq)-100:]
	}

	d.callCounts[toolName]++
	d.checkGenericRepeat(toolName)
	d.checkPollPattern(toolName, input)
	d.checkPingPong(toolName)
	d.checkGlobalBreaker()
}

// checkGenericRepeat detects when the same tool is called repeatedly.
func (d *LoopDetector) checkGenericRepeat(toolName string) {
	count := d.callCounts[toolName]

	if count == d.WarningThreshold {
		msg := "warning: " + toolName + " called " + itoa(count) + " times consecutively"
		if d.onWarning != nil {
			d.onWarning(LoopGenericRepeat, count, msg)
		}
	}

	if count == d.CriticalThreshold {
		msg := "critical: " + toolName + " loop detected (" + itoa(count) + " iterations)"
		if d.onCritical != nil {
			d.onCritical(LoopGenericRepeat, count, msg)
		}
	}
}

// checkPollPattern detects polling patterns (repeated calls with similar inputs).
func (d *LoopDetector) checkPollPattern(toolName string, input map[string]any) {
	// Simple heuristic: same tool called many times suggests polling
	if toolName == "Read" || toolName == "Bash" || toolName == "Glob" {
		d.pollPatterns[toolName]++
		if d.pollPatterns[toolName] == d.WarningThreshold {
			if d.onWarning != nil {
				msg := "warning: possible polling detected with " + toolName
				d.onWarning(LoopPollNoProgress, d.pollPatterns[toolName], msg)
			}
		}
	}
}

// checkPingPong detects back-and-forth patterns between two tools.
func (d *LoopDetector) checkPingPong(toolName string) {
	if len(d.toolSeq) < 2 {
		return
	}
	last := d.toolSeq[len(d.toolSeq)-2]
	pair := last + "->" + toolName
	d.pingPongPairs[pair]++

	if d.pingPongPairs[pair] >= d.WarningThreshold {
		if d.onWarning != nil {
			msg := "warning: ping-pong pattern detected: " + pair
			d.onWarning(LoopPingPong, d.pingPongPairs[pair], msg)
		}
	}
}

// checkGlobalBreaker checks total call count.
func (d *LoopDetector) checkGlobalBreaker() {
	if d.globalCount >= d.GlobalThreshold {
		if d.onCritical != nil {
			msg := "global circuit breaker: " + itoa(d.globalCount) + " total tool calls"
			d.onCritical(LoopGlobalCircuitBreaker, d.globalCount, msg)
		}
	}
}

// RecordProgress marks a successful turn (non-tool-use).
func (d *LoopDetector) RecordProgress() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastProgress = time.Now()
	d.resetCounts()
}

// resetCounts resets per-turn counters after a successful turn.
//
// signatureWindow + signatureTripped intentionally NOT reset: a
// model that ended one turn with `tool_use` then stops in the next
// turn shouldn't be punished, but a model that produces a textual
// summary as a fake "progress" turn and then re-enters the same loop
// must still trip. globalCount is also preserved so the GlobalThreshold
// circuit breaker spans the whole session.
func (d *LoopDetector) resetCounts() {
	d.callCounts = make(map[string]int)
	d.pollPatterns = make(map[string]int)
	d.pingPongPairs = make(map[string]int)
}

// RecordStep ingests a completed step (tool batch + matching results)
// and updates the sliding-window signature detector. Pairs each
// `tool_use` ContentBlock with its matching `tool_result` (by
// ToolUseID) and folds them into a SHA-256 signature; identical
// signatures appearing > SignatureMaxRepeats times within the window
// flip ShouldAbort to true.
//
// Signature design choice (mirroring crush): include result body, not
// just the call. That way a model that retries a failing command
// gets a different signature on each call — only when the call AND
// the failure mode are byte-identical does it count as a repeat.
// Pure-retry-with-progress (e.g., reading a file that someone is
// still writing) doesn't false-trip.
func (d *LoopDetector) RecordStep(toolUses, results []provider.ContentBlock) {
	sig := stepSignature(toolUses, results)
	if sig == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	d.signatureWindow = append(d.signatureWindow, sig)
	if len(d.signatureWindow) > d.SignatureWindowSize {
		d.signatureWindow = d.signatureWindow[len(d.signatureWindow)-d.SignatureWindowSize:]
	}

	// Crush requires the window to fill before detecting; we don't,
	// because the user's loops the safety net most needs to catch
	// (cd … && git rebase --continue retries) reach the repeat
	// threshold long before 10 distinct steps accumulate. Whatever
	// fits in the window is fair game.
	counts := make(map[string]int, len(d.signatureWindow))
	for _, s := range d.signatureWindow {
		counts[s]++
		if counts[s] > d.SignatureMaxRepeats {
			d.signatureTripped = true
			if d.onCritical != nil {
				msg := "signature loop detected: same tool call + result repeated " +
					itoa(counts[s]) + " times in window of " +
					itoa(d.SignatureWindowSize)
				d.onCritical(LoopSignatureRepeat, counts[s], msg)
			}
			return
		}
	}
}

// ShouldAbort returns true if loops have been detected and user hasn't overridden.
func (d *LoopDetector) ShouldAbort() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.signatureTripped {
		return true
	}
	return d.globalCount >= d.GlobalThreshold
}

// AbortReason returns a short kind tag for whichever rule fired.
// Empty string when ShouldAbort would currently return false.
func (d *LoopDetector) AbortReason() LoopDetectorKind {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.signatureTripped {
		return LoopSignatureRepeat
	}
	if d.globalCount >= d.GlobalThreshold {
		return LoopGlobalCircuitBreaker
	}
	return ""
}

// stepSignature pairs tool_use blocks with tool_result blocks (by
// ToolUseID) and returns the hex-encoded SHA-256 of their
// stable-marshalled concatenation. Returns "" when there are no tool
// calls in the step (text-only assistant turns can't loop).
func stepSignature(toolUses, results []provider.ContentBlock) string {
	calls := filterByType(toolUses, "tool_use")
	if len(calls) == 0 {
		return ""
	}
	resultByID := make(map[string]string, len(results))
	for _, r := range results {
		if r.Type == "tool_result" {
			resultByID[r.ToolUseID] = r.ToolResult
		}
	}
	h := sha256.New()
	for _, c := range calls {
		io.WriteString(h, c.ToolName)
		_, _ = h.Write([]byte{0})
		io.WriteString(h, stableJSON(c.ToolInput))
		_, _ = h.Write([]byte{0})
		io.WriteString(h, resultByID[c.ToolUseID])
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func filterByType(blocks []provider.ContentBlock, t string) []provider.ContentBlock {
	out := make([]provider.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == t {
			out = append(out, b)
		}
	}
	return out
}

// stableJSON marshals m with sorted keys so two equivalent inputs
// always produce the same signature. Falls back to "" on error;
// callers treat empty-input as "no signature contribution".
func stableJSON(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type pair struct {
		K string
		V any
	}
	pairs := make([]pair, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, pair{k, m[k]})
	}
	b, err := json.Marshal(pairs)
	if err != nil {
		return ""
	}
	return string(b)
}

// Stats returns current loop detection statistics.
func (d *LoopDetector) Stats() LoopStats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return LoopStats{
		GlobalCount:  d.globalCount,
		ToolCounts:   d.callCounts,
		LastProgress: d.lastProgress,
	}
}

// LoopStats holds loop detection statistics.
type LoopStats struct {
	GlobalCount  int            `json:"global_count"`
	ToolCounts   map[string]int `json:"tool_counts"`
	LastProgress time.Time      `json:"last_progress"`
}

// OnWarning sets the warning callback.
func (d *LoopDetector) OnWarning(f func(kind LoopDetectorKind, count int, msg string)) {
	d.onWarning = f
}

// OnCritical sets the critical callback.
func (d *LoopDetector) OnCritical(f func(kind LoopDetectorKind, count int, msg string)) {
	d.onCritical = f
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
