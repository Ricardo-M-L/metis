package tui

// render_cache_test.go — covers the cache's contract end-to-end:
//   * basic hit/miss
//   * width / role / content / expand all participate in the key
//   * Kind=="start" is never cached
//   * dedupe (×N) content mutation correctly misses
//   * InvalidateAll clears both maps but keeps counters
//   * contentCacheKey switches to length+xxh3 above the threshold
//   * RecordRender emits "slow render" above the threshold
//   * RecordView emits periodic stats every statsLogEvery frames
//
// Construction note: these tests build small Message / ToolEvent values
// and exercise only the cache layer. The render functions themselves
// (renderMessage / renderToolEvent) are not invoked here — they remain
// covered by tool_render_test.go / banner_test.go / scroll_test.go.

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// withCapturedSlog redirects the default slog logger to a string builder
// for the duration of the test. Returns the captured buffer + a restore
// func. We need this because our slow-render and stats logs go through
// slog.Debug — the test must observe them without depending on the
// process-wide global state of stderr.
func withCapturedSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	return &buf, func() { slog.SetDefault(prev) }
}

func TestRenderCache_HitMiss(t *testing.T) {
	c := newRenderCache(8, 100)
	m := Message{Role: "user", Content: "hi"}

	if _, ok := c.GetMessage(m, 80); ok {
		t.Fatal("first Get on empty cache should miss")
	}
	c.PutMessage(m, 80, "rendered hi")

	s, ok := c.GetMessage(m, 80)
	if !ok || s != "rendered hi" {
		t.Fatalf("after Put, Get should return the stored value: got %q ok=%v", s, ok)
	}

	hits, miss, _ := c.Stats()
	if hits != 1 || miss != 1 {
		t.Errorf("counters: hits=%d miss=%d, want hits=1 miss=1", hits, miss)
	}
}

func TestRenderCache_WidthDifferentKey(t *testing.T) {
	c := newRenderCache(8, 100)
	m := Message{Role: "assistant", Content: "body text"}

	c.PutMessage(m, 80, "rendered@80")
	c.PutMessage(m, 81, "rendered@81")

	if s, _ := c.GetMessage(m, 80); s != "rendered@80" {
		t.Errorf("width 80: got %q want rendered@80", s)
	}
	if s, _ := c.GetMessage(m, 81); s != "rendered@81" {
		t.Errorf("width 81: got %q want rendered@81", s)
	}
	if _, ok := c.GetMessage(m, 100); ok {
		t.Error("width 100 was never put — expected miss")
	}
}

func TestRenderCache_ContentChangeMisses(t *testing.T) {
	c := newRenderCache(8, 100)
	base := Message{Role: "error", Content: "API Error: foo"}
	c.PutMessage(base, 80, "rendered base")

	// Simulate the dedupe (×N) path that mutates m.messages[i].Content
	// in place — same role + same width but different content must miss.
	deduped := Message{Role: "error", Content: "API Error: foo (×2)"}
	if _, ok := c.GetMessage(deduped, 80); ok {
		t.Error("post-dedupe content should miss")
	}
}

func TestRenderCache_ToolStartNotCached(t *testing.T) {
	c := newRenderCache(8, 100)
	te := ToolEvent{
		Kind:     "start",
		ToolName: "Read",
		Input:    map[string]any{"path": "/x"},
	}

	// Put should be a no-op for start events.
	c.PutTool(te, false, 80, "rendered start")
	if s, ok := c.GetTool(te, false, 80); ok {
		t.Errorf("Kind=start should never hit, got %q ok=%v", s, ok)
	}
	// Sanity: counters reflect the two miss paths (Put no-op and Get miss).
	_, miss, _ := c.Stats()
	if miss != 1 {
		t.Errorf("expected exactly one miss recorded, got %d", miss)
	}
}

func TestRenderCache_ToolResultCached(t *testing.T) {
	c := newRenderCache(8, 100)
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Output:   "10 lines",
		IsError:  false,
	}

	c.PutTool(te, false, 80, "rendered result")
	s, ok := c.GetTool(te, false, 80)
	if !ok || s != "rendered result" {
		t.Errorf("expected hit on result, got %q ok=%v", s, ok)
	}
}

func TestRenderCache_ExpandTogglesMiss(t *testing.T) {
	c := newRenderCache(8, 100)
	te := ToolEvent{Kind: "result", ToolName: "Read", Output: "x"}

	c.PutTool(te, false, 80, "compact")
	c.PutTool(te, true, 80, "expanded")

	if s, _ := c.GetTool(te, false, 80); s != "compact" {
		t.Errorf("expand=false: got %q want compact", s)
	}
	if s, _ := c.GetTool(te, true, 80); s != "expanded" {
		t.Errorf("expand=true: got %q want expanded", s)
	}
}

func TestRenderCache_IsErrorTogglesMiss(t *testing.T) {
	// Same tool name + output but different IsError must produce
	// distinct cache entries — the renderer styles error and success
	// rows differently (red ✗ vs accent ✓).
	c := newRenderCache(8, 100)
	teOK := ToolEvent{Kind: "result", ToolName: "Bash", Output: "done"}
	teErr := ToolEvent{Kind: "result", ToolName: "Bash", Output: "done", IsError: true}

	c.PutTool(teOK, false, 80, "ok render")
	c.PutTool(teErr, false, 80, "err render")

	if s, _ := c.GetTool(teOK, false, 80); s != "ok render" {
		t.Errorf("ok-path: got %q", s)
	}
	if s, _ := c.GetTool(teErr, false, 80); s != "err render" {
		t.Errorf("err-path: got %q", s)
	}
}

func TestRenderCache_InvalidateAll(t *testing.T) {
	c := newRenderCache(8, 100)
	c.PutMessage(Message{Role: "user", Content: "x"}, 80, "v1")
	c.PutTool(ToolEvent{Kind: "result", ToolName: "Read", Output: "y"}, false, 80, "v2")

	// Run Get once before invalidate so we have hit/miss baselines.
	c.GetMessage(Message{Role: "user", Content: "x"}, 80)
	hitsBefore, _, _ := c.Stats()
	if hitsBefore == 0 {
		t.Fatal("expected at least one hit before invalidate")
	}

	c.InvalidateAll()

	if _, ok := c.GetMessage(Message{Role: "user", Content: "x"}, 80); ok {
		t.Error("message map should be empty after InvalidateAll")
	}
	if _, ok := c.GetTool(ToolEvent{Kind: "result", ToolName: "Read", Output: "y"}, false, 80); ok {
		t.Error("tool map should be empty after InvalidateAll")
	}

	// Counters must survive — hit-rate metric stays comparable across resizes.
	hitsAfter, _, _ := c.Stats()
	if hitsAfter != hitsBefore {
		t.Errorf("InvalidateAll must preserve hits counter (was %d, now %d)", hitsBefore, hitsAfter)
	}
}

func TestContentCacheKey_SmallStringIdentity(t *testing.T) {
	s := "hello world"
	if k := contentCacheKey(s); k != s {
		t.Errorf("short content should be its own key: got %q want %q", k, s)
	}
}

func TestContentCacheKey_LargeStringHash(t *testing.T) {
	big := strings.Repeat("x", cacheHashThreshold)
	k := contentCacheKey(big)
	if !strings.HasPrefix(k, "h:") {
		t.Errorf("long content key should start with 'h:': got %q", k)
	}
	wantLen := fmt.Sprintf(":%d:", len(big))
	if !strings.Contains(k, wantLen) {
		t.Errorf("long content key %q should contain length marker %q", k, wantLen)
	}
	// Determinism: same input must produce same key.
	if k2 := contentCacheKey(big); k != k2 {
		t.Errorf("contentCacheKey is non-deterministic: %q vs %q", k, k2)
	}
}

func TestContentCacheKey_HashCollisionRare(t *testing.T) {
	// Different lengths defending against any 64-bit hash collision:
	// keys must differ on length even before the hash.
	a := strings.Repeat("a", cacheHashThreshold+10)
	b := strings.Repeat("a", cacheHashThreshold+11)
	ka := contentCacheKey(a)
	kb := contentCacheKey(b)
	if ka == kb {
		t.Errorf("keys must differ when lengths differ: %s vs %s", ka, kb)
	}
}

func TestSlowRender_LogsAboveThreshold(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	c := newRenderCache(5, 100) // 5ms threshold
	c.RecordRender("user", 100, 10*time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, "slow render") {
		t.Errorf("expected 'slow render' in log: %q", out)
	}
	if !strings.Contains(out, "ms=10") {
		t.Errorf("expected 'ms=10' in log: %q", out)
	}

	// Below threshold must not log.
	buf.Reset()
	c.RecordRender("user", 100, 1*time.Millisecond)
	if strings.Contains(buf.String(), "slow render") {
		t.Errorf("fast render should not log: %q", buf.String())
	}
}

func TestStats_PeriodicLogTriggers(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	c := newRenderCache(8, 5) // log every 5 views
	for i := 0; i < 4; i++ {
		c.RecordView()
	}
	if strings.Contains(buf.String(), "render cache stats") {
		t.Errorf("stats should not log before 5 views: %q", buf.String())
	}
	c.RecordView() // 5th view crosses the boundary
	if !strings.Contains(buf.String(), "render cache stats") {
		t.Errorf("expected stats log on 5th view, got %q", buf.String())
	}
}

func TestRecordRender_AccumulatesAvg(t *testing.T) {
	// avgRenderMs in Stats() should reflect the running mean.
	c := newRenderCache(1000, 1000) // disable both log triggers
	c.RecordRender("u", 1, 10*time.Millisecond)
	c.RecordRender("u", 1, 20*time.Millisecond)
	c.RecordRender("u", 1, 30*time.Millisecond)

	_, _, avg := c.Stats()
	const want = 20.0 // ms
	const tol = 0.01
	if avg < want-tol || avg > want+tol {
		t.Errorf("avgRenderMs = %.3f, want ~20.0", avg)
	}
}
