package agent

// cache_stats_test.go — pin the ring buffer + fingerprint shape.
// Live-LLM-driven test of the wiring is part of the e2e tmux harness.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCacheStatsRing_AddAndSnapshot(t *testing.T) {
	r := NewCacheStatsRing(0)
	for i := 1; i <= 5; i++ {
		r.Add(CacheStat{Turn: i, Input: i * 100, CacheRead: i * 50})
	}
	snap := r.Snapshot()
	if len(snap) != 5 {
		t.Errorf("len=%d; want 5", len(snap))
	}
	if snap[0].Turn != 1 || snap[4].Turn != 5 {
		t.Errorf("ordering wrong: %+v", snap)
	}
}

func TestCacheStatsRing_RingBufferEvicts(t *testing.T) {
	r := NewCacheStatsRing(3)
	for i := 1; i <= 7; i++ {
		r.Add(CacheStat{Turn: i})
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Errorf("len=%d; want 3 (cap)", len(snap))
	}
	// Should keep the LAST 3 (turns 5, 6, 7).
	if snap[0].Turn != 5 || snap[2].Turn != 7 {
		t.Errorf("eviction wrong: %+v", snap)
	}
}

func TestCacheStat_HitRate(t *testing.T) {
	s := CacheStat{Input: 100, CacheRead: 200, CacheCreate: 0}
	got := s.HitRate()
	want := 200.0 / 300.0
	if got < want-0.001 || got > want+0.001 {
		t.Errorf("HitRate=%v; want ~%v", got, want)
	}
}

func TestCacheStat_HitRate_NoUsage(t *testing.T) {
	s := CacheStat{}
	if got := s.HitRate(); got != 0 {
		t.Errorf("HitRate on zero usage = %v; want 0", got)
	}
}

func TestCacheStatsRing_HitRateAggregate(t *testing.T) {
	r := NewCacheStatsRing(0)
	r.Add(CacheStat{Input: 100, CacheRead: 100})
	r.Add(CacheStat{Input: 200, CacheRead: 200})
	got := r.HitRate()
	want := 300.0 / 600.0
	if got < want-0.001 || got > want+0.001 {
		t.Errorf("HitRate=%v; want ~%v", got, want)
	}
}

func TestCacheStatsRing_LastBreakDetected(t *testing.T) {
	r := NewCacheStatsRing(0)
	r.Add(CacheStat{Turn: 1, Fingerprint: "abc"})
	r.Add(CacheStat{Turn: 2, Fingerprint: "abc"})
	if oldFP, newFP := r.LastBreak(); oldFP != "" || newFP != "" {
		t.Errorf("expected no break on identical fingerprints; got %s→%s", oldFP, newFP)
	}
	r.Add(CacheStat{Turn: 3, Fingerprint: "xyz"})
	oldFP, newFP := r.LastBreak()
	if oldFP != "abc" || newFP != "xyz" {
		t.Errorf("LastBreak = %s→%s; want abc→xyz", oldFP, newFP)
	}
}

func TestCacheStatsRing_LastBreakNeedsTwoEntries(t *testing.T) {
	r := NewCacheStatsRing(0)
	if oldFP, newFP := r.LastBreak(); oldFP != "" || newFP != "" {
		t.Errorf("empty ring should have no break; got %s→%s", oldFP, newFP)
	}
	r.Add(CacheStat{Fingerprint: "abc"})
	if oldFP, newFP := r.LastBreak(); oldFP != "" || newFP != "" {
		t.Errorf("single-entry ring should have no break; got %s→%s", oldFP, newFP)
	}
}

func TestFingerprintFor_DeterministicAndChanges(t *testing.T) {
	tools := []llm.ToolSpec{{Name: "Bash"}, {Name: "Edit"}}
	fp1 := FingerprintFor("claude", "system text", tools, "low")
	fp2 := FingerprintFor("claude", "system text", tools, "low")
	if fp1 != fp2 {
		t.Errorf("identical inputs should produce identical fingerprints; got %s vs %s", fp1, fp2)
	}
	// Different model → different fingerprint.
	fp3 := FingerprintFor("openai", "system text", tools, "low")
	if fp1 == fp3 {
		t.Errorf("different model should change fingerprint")
	}
	// Different effort → different fingerprint.
	fp4 := FingerprintFor("claude", "system text", tools, "high")
	if fp1 == fp4 {
		t.Errorf("different effort should change fingerprint")
	}
	// Different system text → different fingerprint.
	fp5 := FingerprintFor("claude", "different system", tools, "low")
	if fp1 == fp5 {
		t.Errorf("different system should change fingerprint")
	}
}

func TestFingerprintFor_Length(t *testing.T) {
	fp := FingerprintFor("a", "b", nil, "")
	if len(fp) != 12 {
		t.Errorf("fingerprint length = %d; want 12", len(fp))
	}
}
