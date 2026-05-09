package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempFrecencyHome points config.Home() at a fresh tempdir and
// resets the frecency singleton so each test starts from an empty
// store. Returns the tempdir path so tests can poke at the on-disk
// JSON if they care about persistence.
//
// The eager load() after reset is so direct test pokes at
// frecencySingleton.entries don't hit a nil map (resetFrecencyStore
// nils the map; load() reinitialises it from disk or empty).
func withTempFrecencyHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	resetFrecencyStore()
	frecencySingleton.load()
	t.Cleanup(resetFrecencyStore)
	return dir
}

func TestFrecencyScore_EmptyStoreReturnsZero(t *testing.T) {
	withTempFrecencyHome(t)
	if got := frecencyScore("foo.go"); got != 0 {
		t.Fatalf("empty store should score 0, got %v", got)
	}
}

func TestRecordMentionAccess_IncrementsCount(t *testing.T) {
	withTempFrecencyHome(t)
	RecordMentionAccess("foo.go")
	RecordMentionAccess("foo.go")
	RecordMentionAccess("foo.go")

	frecencySingleton.mu.Lock()
	e := frecencySingleton.entries["foo.go"]
	frecencySingleton.mu.Unlock()
	if e.Count != 3 {
		t.Fatalf("count: want 3, got %d", e.Count)
	}
	if time.Since(e.LastAccessAt) > 5*time.Second {
		t.Fatalf("LastAccessAt should be recent, got %v", e.LastAccessAt)
	}
}

func TestRecordMentionAccess_EmptyPathIgnored(t *testing.T) {
	withTempFrecencyHome(t)
	RecordMentionAccess("")
	frecencySingleton.mu.Lock()
	n := len(frecencySingleton.entries)
	frecencySingleton.mu.Unlock()
	if n != 0 {
		t.Fatalf("empty path should not be recorded, got %d entries", n)
	}
}

func TestFrecencyScore_DecaysWithAge(t *testing.T) {
	withTempFrecencyHome(t)
	RecordMentionAccess("recent.go")

	// Seed an old entry directly (RecordMentionAccess always stamps now).
	frecencySingleton.mu.Lock()
	frecencySingleton.entries["old.go"] = frecencyEntry{
		Count:        1,
		LastAccessAt: time.Now().Add(-12 * time.Hour),
	}
	frecencySingleton.mu.Unlock()

	recent := frecencyScore("recent.go")
	old := frecencyScore("old.go")
	if !(recent > old) {
		t.Fatalf("recent (%v) should outrank 12h-old (%v)", recent, old)
	}
}

func TestFrecencyScore_FloorPreservesAncientEntries(t *testing.T) {
	withTempFrecencyHome(t)
	frecencySingleton.mu.Lock()
	frecencySingleton.entries["ancient.go"] = frecencyEntry{
		Count:        100,
		LastAccessAt: time.Now().Add(-30 * 24 * time.Hour),
	}
	frecencySingleton.mu.Unlock()

	got := frecencyScore("ancient.go")
	// 100 × 0.1 = 10. Anything below 10 means the floor was skipped.
	if got < 9.5 || got > 10.5 {
		t.Fatalf("100-count 30-day-old entry should score ~10 (count × 0.1), got %v", got)
	}
}

func TestSortByFrecency_MostFrequentFirst(t *testing.T) {
	withTempFrecencyHome(t)
	for i := 0; i < 5; i++ {
		RecordMentionAccess("hot.go")
	}
	RecordMentionAccess("warm.go")

	in := []string{"cold.go", "warm.go", "hot.go", "frozen.go"}
	got := sortByFrecency(in)
	want := []string{"hot.go", "warm.go", "cold.go", "frozen.go"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortByFrecency[%d]: want %q got %q (full %v)", i, want[i], got[i], got)
		}
	}
}

func TestSortByFrecency_StableForUntouchedFiles(t *testing.T) {
	withTempFrecencyHome(t)
	in := []string{"a.go", "b.go", "c.go", "d.go"}
	got := sortByFrecency(in)
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("untouched files should keep input order; got %v", got)
		}
	}
}

func TestSortByFrecency_ShortInputPassthrough(t *testing.T) {
	withTempFrecencyHome(t)
	if got := sortByFrecency(nil); got != nil {
		t.Fatalf("nil input should return nil, got %v", got)
	}
	if got := sortByFrecency([]string{"only.go"}); len(got) != 1 || got[0] != "only.go" {
		t.Fatalf("single-element input should pass through, got %v", got)
	}
}

func TestEvictOldestHalf_RemovesHalf(t *testing.T) {
	m := map[string]frecencyEntry{}
	now := time.Now()
	for i := 0; i < 10; i++ {
		m[string(rune('a'+i))] = frecencyEntry{
			Count:        1,
			LastAccessAt: now.Add(time.Duration(i) * time.Hour),
		}
	}
	evictOldestHalf(m)
	if len(m) != 5 {
		t.Fatalf("expected 5 entries after eviction, got %d", len(m))
	}
	// 'a' (oldest) must be gone, 'j' (newest) must remain.
	if _, ok := m["a"]; ok {
		t.Fatal("oldest entry 'a' should have been evicted")
	}
	if _, ok := m["j"]; !ok {
		t.Fatal("newest entry 'j' should have been retained")
	}
}

func TestRecordMentionAccess_PersistsToDisk(t *testing.T) {
	dir := withTempFrecencyHome(t)
	RecordMentionAccess("foo.go")
	RecordMentionAccess("bar.go")
	RecordMentionAccess("foo.go")

	path := filepath.Join(dir, "atmention-frecency.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("frecency file should exist: %v", err)
	}
	var got map[string]frecencyEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("frecency file should be valid JSON: %v", err)
	}
	if got["foo.go"].Count != 2 {
		t.Fatalf("foo.go count: want 2, got %d", got["foo.go"].Count)
	}
	if got["bar.go"].Count != 1 {
		t.Fatalf("bar.go count: want 1, got %d", got["bar.go"].Count)
	}
}

func TestRecordMentionAccess_RoundtripsAcrossReload(t *testing.T) {
	withTempFrecencyHome(t)
	RecordMentionAccess("survivor.go")
	RecordMentionAccess("survivor.go")

	// Force a reload: drop in-memory state, keep on-disk file.
	resetFrecencyStore()

	got := frecencyScore("survivor.go")
	if got <= 0 {
		t.Fatalf("score should survive reload, got %v", got)
	}
}

func TestRecordMentionAccess_FilePermissionsAreOwnerOnly(t *testing.T) {
	dir := withTempFrecencyHome(t)
	RecordMentionAccess("foo.go")

	info, err := os.Stat(filepath.Join(dir, "atmention-frecency.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("frecency file should be 0o600 (owner read/write only), got %o", mode)
	}
}

func TestFrecencyStore_LoadHandlesCorruptFile(t *testing.T) {
	dir := withTempFrecencyHome(t)
	if err := os.WriteFile(filepath.Join(dir, "atmention-frecency.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetFrecencyStore()
	// Should not panic; should return empty store, not error out.
	if got := frecencyScore("anything.go"); got != 0 {
		t.Fatalf("corrupt file should yield empty store; got score %v", got)
	}
}

func TestFrecencyStore_EvictsAtCap(t *testing.T) {
	withTempFrecencyHome(t)
	// Pre-seed past the cap to avoid 1001 disk writes.
	frecencySingleton.load()
	frecencySingleton.mu.Lock()
	now := time.Now()
	for i := 0; i < frecencyMaxEntries; i++ {
		// Older entries get older timestamps so eviction targets them.
		frecencySingleton.entries[fakePath(i)] = frecencyEntry{
			Count:        1,
			LastAccessAt: now.Add(-time.Duration(frecencyMaxEntries-i) * time.Minute),
		}
	}
	frecencySingleton.mu.Unlock()

	RecordMentionAccess("trigger.go") // pushes count to cap+1, triggers eviction

	frecencySingleton.mu.Lock()
	n := len(frecencySingleton.entries)
	_, hasNewest := frecencySingleton.entries["trigger.go"]
	_, hasOldest := frecencySingleton.entries[fakePath(0)]
	frecencySingleton.mu.Unlock()

	if n > frecencyMaxEntries {
		t.Fatalf("post-eviction size should be ≤ cap %d, got %d", frecencyMaxEntries, n)
	}
	if !hasNewest {
		t.Fatal("just-recorded entry should survive eviction")
	}
	if hasOldest {
		t.Fatal("oldest pre-seeded entry should have been evicted")
	}
}

func fakePath(i int) string {
	return "file_" + itoaSmall(i) + ".go"
}

// itoaSmall — strconv.Itoa would work but we keep imports minimal.
func itoaSmall(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
