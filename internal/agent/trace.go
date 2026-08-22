package agent

// trace.go — optional process-wide event trace hook.
//
// Every Event produced by any Loop (main loop AND sub-agent loops) is
// passed to the hook after it has been emitted to the consumer channel.
// The hook is a synchronous callback on the emitting goroutine; a heavy
// hook adds latency to every turn, so the consumer (session.TraceStore)
// must be cheap (buffered write / background flush).
//
// It is set once at runtime assembly (CLI boot, web server boot) and
// never cleared. Sub-agent loops emit with their own ParentID, so the
// trace captures the full spawn-tree across agents.

import "sync"

var (
	traceHookMu sync.RWMutex
	traceHook   func(Event)
)

// SetTraceHook installs the process-wide event trace hook. Pass nil to
// remove. The latest install wins; this is process wiring (set once at
// runtime assembly), not per-session state.
func SetTraceHook(fn func(Event)) {
	traceHookMu.Lock()
	defer traceHookMu.Unlock()
	traceHook = fn
}

// notifyTraceHook forwards ev to the installed hook, if any. Called by
// emit after the event has been enqueued for the consumer channel.
// Runs synchronously on the emitting goroutine; the TraceAdapter's own
// mutex serializes SetSession vs OnEvent, so a flushTextLocked race is
// impossible.
func notifyTraceHook(ev Event) {
	traceHookMu.RLock()
	fn := traceHook
	traceHookMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}
