package tui

// perf_config.go — runtime-tunable TUI performance settings, sourced
// from `[ui.performance]` in config.toml. We keep a package-level
// snapshot so per-tick lookups (tickInterval, eventBufferSize) don't
// have to reach into config every redraw.
//
// SetPerfConfig is called by setupRuntime after config.Load() succeeds.
// Tests inject by setting perfCfg directly.

import (
	"os"
	"strconv"
	"sync"
)

// PerfConfig is the read-only view of the tunables this package
// consumes. Mirrors config.UIPerformance but lives here to avoid a
// hard dependency from internal/tui → internal/config (which would
// create an import cycle once the agent loop uses TUI helpers).
type PerfConfig struct {
	TickMs          int
	EventBufferSize int
	MouseWheelLines int
	ReducedMotion   bool
	// SlowRenderMs / StatsLogEvery feed render_cache.go's instrument.
	// 0 means "use the package default" (8 ms / 100 frames).
	SlowRenderMs  int
	StatsLogEvery int
	// MaxMountedItems caps how many chat items the virtualized list
	// physically holds. claude-code uses 300 to bound fiber alloc /
	// per-frame work regardless of session length. 0 = unbounded
	// (existing behavior). When the cap is hit, oldest messages stop
	// being fed to the list — the chat surface still has them in
	// m.messages, they just don't render.
	MaxMountedItems int
	// ScrollQuantum quantizes mouse-wheel events: only every N lines'
	// worth of accumulated wheel delta triggers a list.ScrollBy call,
	// reducing the per-frame churn from a trackpad spamming wheel
	// events. claude-code uses SCROLL_QUANTUM=40. 0 = no quantization
	// (every wheel event scrolls; existing behavior).
	ScrollQuantum int
}

var (
	perfMu  sync.RWMutex
	perfCfg = PerfConfig{
		// Mirror the defaults() in internal/config so a TUI started
		// without ever calling SetPerfConfig still gets sane values.
		TickMs:          40,
		EventBufferSize: 256,
		MouseWheelLines: 1,
	}
)

// SetPerfConfig swaps the package snapshot. Called once at startup
// from setupRuntime; safe to call again (e.g. for a future /reload
// command).
func SetPerfConfig(p PerfConfig) {
	perfMu.Lock()
	defer perfMu.Unlock()
	perfCfg = p
}

// perfConfig returns a copy of the current snapshot. Cheap; the
// per-tick callers (tickInterval) and per-frame callers (View) hit
// this often.
func perfConfig() PerfConfig {
	perfMu.RLock()
	defer perfMu.RUnlock()
	return perfCfg
}

// eventBufferSize is the depth used for new agent.Event channels.
// Same precedence as tickInterval: env > config > default.
func eventBufferSize() int {
	if v := os.Getenv("METIS_EVENT_BUFFER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 16 && n <= 16384 {
			return n
		}
	}
	if cfg := perfConfig(); cfg.EventBufferSize >= 16 && cfg.EventBufferSize <= 16384 {
		return cfg.EventBufferSize
	}
	return 256
}

// mouseWheelLines is how many transcript lines to scroll per wheel
// detent. 1 = pixel-precise (default); 3 = browser-like jumpy.
func mouseWheelLines() int {
	if v := os.Getenv("METIS_MOUSE_WHEEL_LINES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 50 {
			return n
		}
	}
	if cfg := perfConfig(); cfg.MouseWheelLines >= 1 && cfg.MouseWheelLines <= 50 {
		return cfg.MouseWheelLines
	}
	return 1
}
