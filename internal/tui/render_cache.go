package tui

// render_cache.go — per-item rendered-string cache for the chat surface.
//
// Why this exists: View() runs every spinner tick (25fps). The previous
// implementation re-ran renderMessage / renderToolEvent for every item in
// the timeline on every frame, which meant every assistant reply paid the
// full glamour markdown cost (~3ms per message) every 40ms. A 100-message
// session spent ~300ms/frame doing work it had already done. This cache
// flips that to O(N) work amortized — each message renders once and then
// hits a map until its width changes (resize) or its content mutates
// (error dedupe ×N).
//
// Design notes:
//   - Cache keys are constructed from immutable fields. Go strings make
//     map keys cheap (pointer-shared, no copy), and a small-content
//     fast-path avoids paying any hash cost for the typical short message.
//   - Large content (>= cacheHashThreshold bytes) switches to a
//     length+xxh3 digest so a 50KB pasted code block doesn't bloat the
//     map's key memory. crush uses xxh3 the same way.
//   - ToolEvent Kind=="start" is short-lived (it mutates to "result"
//     within ms) and is skipped — caching it would just produce stale
//     entries that get invalidated on the next event.
//   - Streaming text (m.streamingText / m.thinkingText) is rendered
//     outside the timeline path, so it's naturally not cached.
//   - WindowSizeMsg invokes InvalidateAll(); the running counters are
//     preserved so hit-rate stats stay comparable across resizes.
//
// Instrument: claude-code's render-to-screen.ts logs "Slow render:
// …ms" lines and dumps cumulative timing every LOG_EVERY=20 frames.
// We do the equivalent at slog.Debug level — costs nothing when the user
// is on default INFO, surfaces detailed render telemetry when they
// flip METIS_LOG_LEVEL=debug.

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"
)

// cacheHashThreshold — content shorter than this is its own map key
// (zero-cost: Go strings are immutable refs). Above it we hash. 4 KB
// covers the typical assistant reply and the typical tool output without
// triggering hashing; large code paste / tool dump is the case worth
// optimizing.
const cacheHashThreshold = 4096

// messageKey identifies a cached renderMessage(msg, width, expand)
// output.
//
// Only role + content + width + expand are consulted: renderMessage
// doesn't read any other field of Message (Timestamp / ToolName /
// ToolError don't affect the rendered glyph for the roles we cache
// here). The expand bit invalidates the cache when Ctrl+O toggles —
// folded vs. unfolded thinking renders different output.
type messageKey struct {
	role     string
	contentK string // raw content if short, "h:len:xxh3hex" if long
	width    int
	expand   bool
}

// toolKey identifies a cached renderToolEvent(te, expand) output.
//
// renderToolEvent doesn't take a width parameter — its lipgloss styling
// uses the terminal default — so width is not part of this key. If
// renderToolEvent ever grows a width parameter, add it here.
//
// Only Kind=="result" entries are stored. "start" is ephemeral and
// skipped at Get/Put so the View() call site stays branch-free.
type toolKey struct {
	name    string
	kind    string // always "result" in practice
	outputK string // mixed string/hash like contentK
	isError bool
	expand  bool
}

// renderCache is the per-Model cache. Concurrent-safe so future
// pre-rendering (a tea.Cmd that pre-warms the cache before View() runs)
// can write without a race; the map mutex is the only contention point
// and the work it guards is a few-byte map lookup.
type renderCache struct {
	mu   sync.RWMutex
	msg  map[messageKey]string
	tool map[toolKey]string

	hits          atomic.Int64
	miss          atomic.Int64
	totalRenderNs atomic.Int64
	putCount      atomic.Int64
	viewCount     atomic.Int64

	// Configurable thresholds (loaded from perfConfig at construction).
	slowRenderMs  int
	statsLogEvery int
}

// Defaults applied when the config layer passes 0/negative values.
const (
	defaultSlowRenderMs  = 8
	defaultStatsLogEvery = 100
)

// newRenderCache builds an empty cache. slowMs and statsEvery are
// soft-clamped: <=0 falls back to the defaults so a missing config
// section can't disable instrument silently.
func newRenderCache(slowMs, statsEvery int) *renderCache {
	if slowMs <= 0 {
		slowMs = defaultSlowRenderMs
	}
	if statsEvery <= 0 {
		statsEvery = defaultStatsLogEvery
	}
	return &renderCache{
		msg:           make(map[messageKey]string),
		tool:          make(map[toolKey]string),
		slowRenderMs:  slowMs,
		statsLogEvery: statsEvery,
	}
}

// contentCacheKey derives a stable cache-key fragment for arbitrary
// content. Short content is its own key (Go strings are immutable,
// ref-shared, so this is essentially free). Content above the threshold
// gets a fixed-width "h:<len>:<xxh3 hex>" digest so 50KB blobs don't
// inflate the map's key memory.
//
// The length prefix is a defense against the (cosmically rare) xxh3-64
// collision: two different contents hashing to the same 64-bit value
// would still need matching byte-lengths to collide here, which makes
// the practical collision probability vanishingly small even across a
// long-lived process.
func contentCacheKey(content string) string {
	if len(content) < cacheHashThreshold {
		return content
	}
	return fmt.Sprintf("h:%d:%x", len(content), xxh3.HashString(content))
}

// GetMessage looks up a previously rendered message. Updates hit/miss
// counters; the (s, ok) shape matches the standard Go map idiom so the
// View() call site reads naturally.
//
// nil-safe: tests that build a Model literal without going through
// NewModel (newScrollTestModel) get c==nil, which we treat as "always
// miss, never record" so View() falls back to recomputing every frame
// (the pre-cache behavior). No production code path produces a nil
// renderCache — NewModel always populates it.
func (c *renderCache) GetMessage(m Message, width int, expand bool) (string, bool) {
	if c == nil {
		return "", false
	}
	k := messageKey{role: m.Role, contentK: contentCacheKey(m.Content), width: width, expand: expand}
	c.mu.RLock()
	s, ok := c.msg[k]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
	} else {
		c.miss.Add(1)
	}
	return s, ok
}

// PutMessage stores a freshly rendered message. Caller is responsible
// for measuring the render cost via RecordRender — keeping that out of
// Put() means callers that already have the rendered string (e.g. tests)
// don't pay for fake telemetry.
func (c *renderCache) PutMessage(m Message, width int, expand bool, rendered string) {
	if c == nil {
		return
	}
	k := messageKey{role: m.Role, contentK: contentCacheKey(m.Content), width: width, expand: expand}
	c.mu.Lock()
	c.msg[k] = rendered
	c.mu.Unlock()
}

// GetTool returns the cached render for an existing tool event.
// Kind=="start" is a guaranteed miss (no map lookup): start-state
// events flip to "result" within ms, so caching them would just yield
// stale entries.
func (c *renderCache) GetTool(te ToolEvent, expand bool, width int) (string, bool) {
	if c == nil {
		return "", false
	}
	if te.Kind == "start" {
		c.miss.Add(1)
		return "", false
	}
	k := toolKey{
		name:    te.ToolName,
		kind:    te.Kind,
		outputK: contentCacheKey(te.Output),
		isError: te.IsError,
		expand:  expand,
	}
	_ = width // reserved for future renderToolEvent signature growth
	c.mu.RLock()
	s, ok := c.tool[k]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
	} else {
		c.miss.Add(1)
	}
	return s, ok
}

// PutTool stores a rendered tool event. Kind=="start" is a no-op (see
// GetTool) so the View() call site doesn't need a guard.
func (c *renderCache) PutTool(te ToolEvent, expand bool, width int, rendered string) {
	if c == nil {
		return
	}
	if te.Kind == "start" {
		return
	}
	k := toolKey{
		name:    te.ToolName,
		kind:    te.Kind,
		outputK: contentCacheKey(te.Output),
		isError: te.IsError,
		expand:  expand,
	}
	_ = width
	c.mu.Lock()
	c.tool[k] = rendered
	c.mu.Unlock()
}

// RecordRender accumulates render-cost telemetry. Emits a "slow render"
// log line when a single call exceeds the threshold (claude-code parity:
// render-to-screen.ts emits "Slow render: …ms" at the same boundary)
// and feeds the rolling average that RecordView prints periodically.
func (c *renderCache) RecordRender(role string, contentLen int, elapsed time.Duration) {
	if c == nil {
		return
	}
	ms := elapsed.Milliseconds()
	if ms >= int64(c.slowRenderMs) {
		slog.Debug("slow render",
			"role", role,
			"content_len", contentLen,
			"ms", ms,
		)
	}
	c.totalRenderNs.Add(elapsed.Nanoseconds())
	c.putCount.Add(1)
}

// RecordView is called once per View() invocation. Every statsLogEvery
// frames it dumps cumulative cache stats — claude-code's
// render-to-screen.ts does the same with LOG_EVERY=20.
func (c *renderCache) RecordView() {
	if c == nil {
		return
	}
	n := c.viewCount.Add(1)
	if c.statsLogEvery <= 0 || n%int64(c.statsLogEvery) != 0 {
		return
	}
	h := c.hits.Load()
	miss := c.miss.Load()
	var hitRate float64
	if total := h + miss; total > 0 {
		hitRate = float64(h) / float64(total)
	}
	var avgRenderMs float64
	if pc := c.putCount.Load(); pc > 0 {
		avgRenderMs = float64(c.totalRenderNs.Load()) / float64(pc) / 1e6
	}
	slog.Debug("render cache stats",
		"view_count", n,
		"hits", h,
		"miss", miss,
		"hit_rate", hitRate,
		"avg_render_ms", avgRenderMs,
	)
}

// InvalidateAll clears both maps. Counters are intentionally preserved
// so hit-rate metrics remain comparable across resize events.
func (c *renderCache) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.msg = make(map[messageKey]string)
	c.tool = make(map[toolKey]string)
	c.mu.Unlock()
}

// Stats exposes running counters for tests and external instrumentation.
// avgRenderMs is the mean per-render cost across the entire process
// lifetime (cleared only by recreating the cache, not by InvalidateAll).
func (c *renderCache) Stats() (hits, miss int64, avgRenderMs float64) {
	if c == nil {
		return 0, 0, 0
	}
	hits = c.hits.Load()
	miss = c.miss.Load()
	if pc := c.putCount.Load(); pc > 0 {
		avgRenderMs = float64(c.totalRenderNs.Load()) / float64(pc) / 1e6
	}
	return
}
