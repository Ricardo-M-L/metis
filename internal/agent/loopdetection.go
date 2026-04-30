// Package agent implements Metis's agent loop with streaming,
// tool execution, and memory integration.
package agent

import (
	"sync"
	"time"
)

// LoopDetectorKind classifies the type of loop detected.
type LoopDetectorKind string

const (
	LoopGenericRepeat       LoopDetectorKind = "generic_repeat"
	LoopPollNoProgress      LoopDetectorKind = "known_poll_no_progress"
	LoopGlobalCircuitBreaker LoopDetectorKind = "global_circuit_breaker"
	LoopPingPong            LoopDetectorKind = "ping_pong"
)

// LoopDetector monitors tool call patterns and detects repetitive behavior.
// Inspired by OpenClaw's tool-loop-detection.ts.
type LoopDetector struct {
	mu sync.RWMutex

	// Thresholds
	WarningThreshold  int
	CriticalThreshold int
	GlobalThreshold  int

	// State
	callCounts    map[string]int        // tool name -> count in current streak
	toolSeq       []string             // recent tool call sequence
	globalCount   int                  // total tool calls this session
	lastProgress  time.Time            // last successful non-tool-use turn
	pollPatterns  map[string]int       // detected polling patterns
	pingPongPairs map[string]int       // ping-pong detection pairs

	// Callbacks
	onWarning  func(kind LoopDetectorKind, count int, msg string)
	onCritical func(kind LoopDetectorKind, count int, msg string)
}

// NewLoopDetector creates a new loop detector with default thresholds.
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		WarningThreshold:  10,
		CriticalThreshold: 20,
		GlobalThreshold:   30,

		callCounts:    make(map[string]int),
		toolSeq:       make([]string, 0, 100),
		pollPatterns:  make(map[string]int),
		pingPongPairs: make(map[string]int),
		lastProgress:  time.Now(),
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
func (d *LoopDetector) resetCounts() {
	d.callCounts = make(map[string]int)
	d.pollPatterns = make(map[string]int)
	d.pingPongPairs = make(map[string]int)
}

// ShouldAbort returns true if loops have been detected and user hasn't overridden.
func (d *LoopDetector) ShouldAbort() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.globalCount >= d.GlobalThreshold
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
