package tui

// view_cache_test.go — integration coverage for the View() ↔ renderCache
// boundary. Unit tests in render_cache_test.go cover the cache's own
// contract; here we exercise the end-to-end path with realistic data
// (100 assistant messages + 50 tool results) and confirm:
//   * miss counter == numMessages+numTools after the cache warms once
//   * subsequent frames are 100% hits (miss counter is stable)
//   * width changes and InvalidateAll force re-render
//
// Note about hit count: the chatList virtualization layer queries
// items multiple times per frame (TotalLineCount, AtBottom,
// VisibleItemIndices, lastOffsetItem, Render — each walks getItem),
// so hits accumulate at a rate higher than 1 per item per frame.
// We assert hit_rate > 99% rather than an exact count.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// newCachedScrollModel mirrors newScrollTestModel but populates the
// render cache so View() exercises the cached path. Kept separate so
// existing scroll tests (which want the cache OFF to test viewport
// behavior in isolation) don't pick up cache side-effects.
func newCachedScrollModel(termW, termH int) *Model {
	m := newScrollTestModel(termW, termH)
	// Use small statsLogEvery so the periodic-log path runs at least
	// once during the 100-frame integration loop — catches log-side-
	// effect regressions at zero extra cost.
	m.renderCache = newRenderCache(8, 50)
	return m
}

func TestView_LongTranscript_StableHitRate(t *testing.T) {
	m := newCachedScrollModel(80, 30)

	base := time.Now()
	const numMessages = 100
	const numTools = 50

	// 100 assistant replies with markdown so glamour actually fires
	// (the cache's primary win is skipping repeated glamour parses).
	for i := 0; i < numMessages; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   fmt.Sprintf("**Reply %d** with markdown:\n\n- item one\n- item two\n\n```go\nfmt.Println(%d)\n```", i, i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	// 50 completed tool events at later timestamps so they merge
	// after the messages in the timeline.
	for i := 0; i < numTools; i++ {
		m.toolEvents = append(m.toolEvents, ToolEvent{
			Kind:      "result",
			ToolName:  "Read",
			Output:    fmt.Sprintf("read tool output sample %d", i),
			Duration:  5 * time.Millisecond,
			StartTime: base.Add(time.Duration(numMessages+i) * time.Millisecond),
		})
	}

	// First frame: every distinct (role, content, width) is a miss
	// the first time it's queried. The chatList may query the same
	// item multiple times per frame (TotalLineCount + Render +
	// AtBottom paths), so 1st-frame hits >= 0 (not necessarily 0)
	// because the second query of the same item within frame 1 hits.
	// What we assert: miss == exactly the number of unique items.
	_ = m.View()
	_, miss1, _ := m.renderCache.Stats()
	if miss1 != int64(numMessages+numTools) {
		t.Errorf("first frame miss=%d, want %d (one per unique item)", miss1, numMessages+numTools)
	}

	// Subsequent 99 frames: items are unchanged, so every Get hits.
	const totalFrames = 100
	for i := 1; i < totalFrames; i++ {
		_ = m.View()
	}

	hitsN, missN, avgMs := m.renderCache.Stats()
	// Miss counter must stay frozen — every item rendered exactly once.
	if missN != miss1 {
		t.Errorf("miss grew from %d to %d across %d frames (expected stable)",
			miss1, missN, totalFrames)
	}
	// Hit rate should be > 99% across the run. Exact hit count depends
	// on how many getItem calls each frame makes (varies with which
	// list-internal walkers fire).
	total := hitsN + missN
	hitRate := float64(hitsN) / float64(total)
	if hitRate < 0.99 {
		t.Errorf("hit rate = %.4f after %d frames, want > 0.99 (hits=%d miss=%d)",
			hitRate, totalFrames, hitsN, missN)
	}
	t.Logf("after %d frames: hits=%d miss=%d hit_rate=%.4f avg_render_ms=%.3f",
		totalFrames, hitsN, missN, hitRate, avgMs)
}

func TestView_WidthChangeForcesRerender(t *testing.T) {
	// Direct call to renderCache.GetMessage rather than going through
	// View() — this isolates the "width is part of the key" contract
	// without dragging in the auto-scroll / viewport-height side
	// effects that View() also performs.
	m := newCachedScrollModel(80, 30)

	m.messages = append(m.messages, Message{
		Role:      "assistant",
		Content:   "hello with **markdown**",
		Timestamp: time.Now(),
	})

	_ = m.View()
	_, miss80, _ := m.renderCache.Stats()
	if miss80 == 0 {
		t.Fatal("first frame should produce at least one miss")
	}

	// Resize: width changes, every existing entry is keyed under the
	// old width. Without InvalidateAll the next View() at width=120
	// should still miss every item (different key).
	m.width = 120
	_ = m.View()
	_, miss120, _ := m.renderCache.Stats()
	if miss120 <= miss80 {
		t.Errorf("width change should produce fresh misses: miss80=%d miss120=%d",
			miss80, miss120)
	}
}

func TestView_InvalidateAllForcesRerender(t *testing.T) {
	m := newCachedScrollModel(80, 30)

	m.messages = append(m.messages, Message{
		Role:      "assistant",
		Content:   "stable content",
		Timestamp: time.Now(),
	})

	_ = m.View() // populates cache
	_, missBefore, _ := m.renderCache.Stats()

	_ = m.View() // should be all hits — confirms baseline
	hits1, missAfter1, _ := m.renderCache.Stats()
	if missAfter1 != missBefore {
		t.Fatalf("baseline broken: miss should not grow on second View, got %d vs %d", missAfter1, missBefore)
	}
	if hits1 == 0 {
		t.Fatal("baseline broken: second View should hit cache at least once")
	}

	m.renderCache.InvalidateAll()
	_ = m.View()
	_, missAfter2, _ := m.renderCache.Stats()
	if missAfter2 <= missAfter1 {
		t.Errorf("InvalidateAll should force re-render: miss before=%d after=%d",
			missAfter1, missAfter2)
	}
}

// populateLongTranscript builds a 100-msg + 50-tool timeline shared by
// the long-transcript benchmarks. Pulled into a helper so the
// cache-on / cache-off variants stay strictly comparable — same
// content, same timestamps, only the renderCache pointer differs.
func populateLongTranscript(m *Model) {
	base := time.Now()
	for i := 0; i < 100; i++ {
		m.messages = append(m.messages, Message{
			Role:      "assistant",
			Content:   fmt.Sprintf("**Reply %d** with markdown:\n\n- item one\n- item two\n\n```go\nfmt.Println(%d)\n```", i, i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	for i := 0; i < 50; i++ {
		m.toolEvents = append(m.toolEvents, ToolEvent{
			Kind:      "result",
			ToolName:  "Read",
			Output:    fmt.Sprintf("read tool output sample %d", i),
			Duration:  5 * time.Millisecond,
			StartTime: base.Add(time.Duration(100+i) * time.Millisecond),
		})
	}
}

// BenchmarkView_LongTranscript measures one View() pass with the cache
// enabled — the post-warm steady-state cost. This is the path the
// production TUI hits every spinner tick once a session has been
// rendering for more than one frame.
func BenchmarkView_LongTranscript(b *testing.B) {
	m := newCachedScrollModel(80, 30)
	populateLongTranscript(m)
	_ = m.View() // warm the cache

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkView_LongTranscript_NoCache is the pre-cache baseline.
// newScrollTestModel leaves renderCache=nil; every renderCache method
// short-circuits to a miss / no-op (see render_cache.go nil-safe
// comments), so View() runs the original "renderMessage / renderToolEvent
// every frame" path. The ratio of NoCache:cached ns/op is the
// concrete win the cache delivers.
func BenchmarkView_LongTranscript_NoCache(b *testing.B) {
	m := newScrollTestModel(80, 30)
	populateLongTranscript(m)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkView_LargePastedContent stresses the contentCacheKey hash
// path: a single 50KB message — well above the 4KB hash threshold —
// rendered repeatedly. Confirms the xxh3 + length digest is fast
// enough that we don't trade glamour cost for hash cost.
func BenchmarkView_LargePastedContent(b *testing.B) {
	m := newCachedScrollModel(80, 30)
	big := strings.Repeat("// big paste line\n", 3000) // ~52KB
	m.messages = append(m.messages, Message{
		Role:      "assistant",
		Content:   big,
		Timestamp: time.Now(),
	})
	_ = m.View() // warm

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

func BenchmarkView_LargePastedContent_NoCache(b *testing.B) {
	m := newScrollTestModel(80, 30)
	big := strings.Repeat("// big paste line\n", 3000)
	m.messages = append(m.messages, Message{
		Role:      "assistant",
		Content:   big,
		Timestamp: time.Now(),
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// populateRealisticUserSession mirrors the user's actual usage pattern:
// 200 conversation turns, each with a ~1.2KB user prompt + ~2KB
// assistant markdown reply + 4 tool result rows. Total: 1200 timeline
// items, ~3 MB of content. This is the scale at which the question
// "does P0 alone suffice or do we need P2 (virtualization)?" gets
// answered with real numbers.
func populateRealisticUserSession(m *Model) {
	base := time.Now()
	const turns = 200
	for round := 0; round < turns; round++ {
		m.messages = append(m.messages, Message{
			Role:      "user",
			Content:   strings.Repeat("user prompt with detail ", 50), // ~1.2 KB
			Timestamp: base.Add(time.Duration(round*10) * time.Millisecond),
		})
		m.messages = append(m.messages, Message{
			Role: "assistant",
			Content: fmt.Sprintf(
				"**Reply %d** to that prompt:\n\n```go\nfunc handler() {\n%s}\n```\n\nKey points:\n- one\n- two\n- three\n\nMore narrative %s",
				round,
				strings.Repeat("    line of code\n", 30),
				strings.Repeat("text ", 60),
			), // ~2 KB markdown
			Timestamp: base.Add(time.Duration(round*10+1) * time.Millisecond),
		})
		for tool := 0; tool < 4; tool++ {
			m.toolEvents = append(m.toolEvents, ToolEvent{
				Kind:      "result",
				ToolName:  "Read",
				Output:    strings.Repeat("output line\n", 20),
				Duration:  5 * time.Millisecond,
				StartTime: base.Add(time.Duration(round*10+2+tool) * time.Millisecond),
			})
		}
	}
}

// BenchmarkView_RealisticUserSession measures the user's actual workload.
// 200 turns × (user + assistant + 4 tools) = 1200 timeline items. With
// the cache enabled, this is the steady-state hot path the spinner
// triggers every tick.
func BenchmarkView_RealisticUserSession(b *testing.B) {
	m := newCachedScrollModel(80, 30)
	populateRealisticUserSession(m)
	_ = m.View() // warm

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkView_RealisticUserSession_NoCache: the pre-cache baseline
// at the same scale. The ratio answers "how much pain were users
// already feeling without the cache?"
func BenchmarkView_RealisticUserSession_NoCache(b *testing.B) {
	m := newScrollTestModel(80, 30)
	populateRealisticUserSession(m)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}
