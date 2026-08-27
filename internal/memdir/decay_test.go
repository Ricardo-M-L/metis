package memdir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCurrentStrength_LegacyFileTreatedAsFresh — files written
// before the decay system existed have Strength=0 and
// LastAccessed="" in their frontmatter. CurrentStrength must NOT
// retroactively decay them — that would silently prune everything
// in `~/.metis/memory/` the moment the first dream cycle runs after
// upgrade. The contract: missing fields → DefaultStrength returned.
func TestCurrentStrength_LegacyFileTreatedAsFresh(t *testing.T) {
	fm := &Frontmatter{} // no Strength, no LastAccessed
	got := fm.CurrentStrength(time.Now())
	if got != DefaultStrength {
		t.Errorf("legacy fm strength = %v, want %v (no retroactive decay)", got, DefaultStrength)
	}
}

// TestCurrentStrength_DecaysOverTime — a memo accessed 7 days ago
// at full strength should now be at exactly DecayFactor (0.9).
// Tests the core math; downstream prune decisions depend on it.
func TestCurrentStrength_DecaysOverTime(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	week := now.Add(-7 * 24 * time.Hour)
	fm := &Frontmatter{
		Strength:     1.0,
		LastAccessed: week.Format(time.RFC3339),
	}
	got := fm.CurrentStrength(now)
	// Allow ±0.005 for the fractional-period linear interp.
	want := 0.9
	if got < want-0.005 || got > want+0.005 {
		t.Errorf("strength after 1 period = %v, want ~%v", got, want)
	}
}

// TestCurrentStrength_MultiplePeriodsCompound — 5 periods should
// give roughly 0.9^5 = 0.59049. Tests the iterative compounding.
func TestCurrentStrength_MultiplePeriodsCompound(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	fivePeriodsAgo := now.Add(-5 * DecayPeriodDays * 24 * time.Hour)
	fm := &Frontmatter{
		Strength:     1.0,
		LastAccessed: fivePeriodsAgo.Format(time.RFC3339),
	}
	got := fm.CurrentStrength(now)
	want := 0.59049
	if got < want-0.01 || got > want+0.01 {
		t.Errorf("strength after 5 periods = %v, want ~%v", got, want)
	}
}

// TestCurrentStrength_FutureLastAccessedNoBoost — clock skew can
// produce LastAccessed > now. Strength must NOT exceed the stored
// value (no "negative decay" boost), else clock skew turns into a
// permanent freshness amplifier.
func TestCurrentStrength_FutureLastAccessedNoBoost(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	fm := &Frontmatter{
		Strength:     0.5,
		LastAccessed: future.Format(time.RFC3339),
	}
	got := fm.CurrentStrength(now)
	if got != 0.5 {
		t.Errorf("future LastAccessed produced strength %v; want stored 0.5 (no boost)", got)
	}
}

func TestCurrentStrength_UsesNewestActivityTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm := &Frontmatter{
		Strength:     1,
		LastUsedAt:   now.Add(-2 * 365 * 24 * time.Hour).Format(time.RFC3339),
		LastAccessed: "not-a-timestamp",
		UpdatedAt:    now.Add(-24 * time.Hour).Format(time.RFC3339),
	}
	got := fm.CurrentStrength(now)
	if got < 0.98 {
		t.Fatalf("recent update lost to stale LastUsedAt: strength=%v, want near 1", got)
	}
}

// TestMarkAccessed_ResetsBothFields — writing a memo should set
// Strength=1.0 and LastAccessed=now, so the decay clock restarts.
func TestMarkAccessed_ResetsBothFields(t *testing.T) {
	fm := &Frontmatter{Strength: 0.2, LastAccessed: "2025-01-01T00:00:00Z"}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	fm.MarkAccessed(now)
	if fm.Strength != DefaultStrength {
		t.Errorf("Strength = %v, want %v", fm.Strength, DefaultStrength)
	}
	if fm.LastAccessed != now.UTC().Format(time.RFC3339) {
		t.Errorf("LastAccessed = %q, want %q", fm.LastAccessed, now.UTC().Format(time.RFC3339))
	}
}

// TestDecayAndPrune_KeepsFreshDeletesStale — end-to-end for short-lived
// project memory:
//   - file A: just-written → kept
//   - file B: 6 months untouched → pruned (decayed to ~0.05)
//   - file C: 4 weeks untouched → kept (0.9^4 = 0.656)
func TestDecayAndPrune_KeepsFreshDeletesStale(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	write := func(name string, lastAccessed time.Time) {
		t.Helper()
		fm := &Frontmatter{
			Name:         name,
			Description:  "test " + name,
			Type:         TypeProject,
			Strength:     1.0,
			LastAccessed: lastAccessed.Format(time.RFC3339),
		}
		out, err := RenderFile(fm, "body of "+name)
		if err != nil {
			t.Fatalf("RenderFile %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(root, name+".md"), out, 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	write("fresh", now)
	write("ancient", now.Add(-180*24*time.Hour)) // ~26 periods → 0.9^26 ≈ 0.065
	write("month-old", now.Add(-28*24*time.Hour))

	res, err := DecayAndPrune(context.Background(), root, now)
	if err != nil {
		t.Fatalf("DecayAndPrune: %v", err)
	}
	if res.Kept != 2 {
		t.Errorf("Kept = %d, want 2 (fresh + month-old)", res.Kept)
	}
	if len(res.Pruned) != 1 || filepath.Base(res.Pruned[0]) != "ancient.md" {
		t.Errorf("Pruned = %v, want [ancient.md]", res.Pruned)
	}
	// Verify files-on-disk match.
	if _, err := os.Stat(filepath.Join(root, "fresh.md")); err != nil {
		t.Errorf("fresh.md should still exist; %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ancient.md")); !os.IsNotExist(err) {
		t.Errorf("ancient.md should be deleted; stat err = %v", err)
	}
}

func TestDecayAndPrune_DurableUserPreferenceSurvives(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, typ := range []MemoryType{TypeUser, TypeFeedback} {
		fm := &Frontmatter{
			Name:         string(typ),
			Description:  "durable " + string(typ),
			Type:         typ,
			Strength:     0.001,
			LastAccessed: now.Add(-3 * 365 * 24 * time.Hour).Format(time.RFC3339),
			LastUsedAt:   now.Add(-3 * 365 * 24 * time.Hour).Format(time.RFC3339),
		}
		out, err := RenderFile(fm, "keep this")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, string(typ)+".md"), out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := DecayAndPrune(context.Background(), root, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pruned) != 0 || res.Kept != 2 {
		t.Fatalf("durable memories must survive by default: %+v", res)
	}
}

func TestDecayAndPrune_ProjectUsesLastUseAndUseCount(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	write := func(name string, useCount int) {
		t.Helper()
		fm := &Frontmatter{
			Name:         name,
			Description:  name,
			Type:         TypeProject,
			Strength:     1,
			LastUsedAt:   now.Add(-180 * 24 * time.Hour).Format(time.RFC3339),
			LastAccessed: now.Add(-180 * 24 * time.Hour).Format(time.RFC3339),
			UseCount:     useCount,
		}
		out, _ := RenderFile(fm, name)
		if err := os.WriteFile(filepath.Join(root, name+".md"), out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one_off", 1)
	write("reused", 20)

	res, err := DecayAndPrune(context.Background(), root, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pruned) != 1 || filepath.Base(res.Pruned[0]) != "one_off.md" {
		t.Fatalf("expected only one-off project memo pruned, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "reused.md")); err != nil {
		t.Fatalf("frequently reused project memo should survive: %v", err)
	}
}

func TestDecayAndPrune_ReferenceIsConservative(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm := &Frontmatter{
		Name: "dashboard", Description: "external dashboard", Type: TypeReference,
		Strength: 0.001, LastUsedAt: now.Add(-364 * 24 * time.Hour).Format(time.RFC3339),
	}
	out, _ := RenderFile(fm, "https://example.invalid")
	if err := os.WriteFile(filepath.Join(root, "reference_dashboard.md"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := DecayAndPrune(context.Background(), root, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || len(res.Pruned) != 0 {
		t.Fatalf("reference should be retained conservatively: %+v", res)
	}
}

func TestDecayAndPrune_ContextExpiresLikeProjectState(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm := &Frontmatter{
		Name: "old context", Description: "one-off operational state", Type: TypeContext,
		Strength: 1, LastUsedAt: now.Add(-180 * 24 * time.Hour).Format(time.RFC3339), UseCount: 1,
	}
	out, _ := RenderFile(fm, "temporary state")
	path := filepath.Join(root, "context_old.md")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := DecayAndPrune(context.Background(), root, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pruned) != 1 || filepath.Base(res.Pruned[0]) != filepath.Base(path) {
		t.Fatalf("stale context should expire like project state: %+v", res)
	}
}

func TestDecayAndPrune_PinnedAndHighConfidenceSurvive(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	ancient := now.Add(-3 * 365 * 24 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name       string
		extra      map[string]any
		confidence float64
		wantPrune  bool
	}{
		{name: "boolean pinned", extra: map[string]any{"pinned": true}, wantPrune: false},
		{name: "string pinned", extra: map[string]any{"pinned": "ON"}, wantPrune: false},
		{name: "high confidence", confidence: HighConfidenceRetentionThreshold, wantPrune: false},
		{name: "ordinary abandoned project", confidence: DefaultConfidence, wantPrune: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm := &Frontmatter{
				Name: "project", Description: "project", Type: TypeProject,
				Strength: 0.001, LastUsedAt: ancient, Confidence: tt.confidence, Extra: tt.extra,
			}
			if got := fm.ShouldPrune(now); got != tt.wantPrune {
				t.Fatalf("ShouldPrune() = %v, want %v", got, tt.wantPrune)
			}
		})
	}
}

// TestDecayAndPrune_EmptyRoot — no files → no error, empty result.
// Guards against a fresh install where ~/.metis/memory/ is missing
// or empty.
func TestDecayAndPrune_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	res, err := DecayAndPrune(context.Background(), root, time.Now())
	if err != nil {
		t.Errorf("empty root should not error; got %v", err)
	}
	if res.Kept != 0 || len(res.Pruned) != 0 {
		t.Errorf("empty root: Kept=%d Pruned=%v, want 0/empty", res.Kept, res.Pruned)
	}
}

// TestDecayAndPrune_ContextCancellation — long memdir + cancelled
// ctx returns early with ctx.Err(), doesn't continue deleting.
func TestDecayAndPrune_ContextCancellation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		fm := &Frontmatter{Name: "n", Description: "d", Type: TypeFeedback}
		out, _ := RenderFile(fm, "body")
		_ = os.WriteFile(filepath.Join(root, "f"+string(rune('a'+i))+".md"), out, 0o600)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DecayAndPrune(ctx, root, time.Now())
	if err == nil {
		t.Error("cancelled ctx should propagate as error")
	}
}
