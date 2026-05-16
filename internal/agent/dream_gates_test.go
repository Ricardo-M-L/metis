package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDreamGates_DisabledReturnsFalse — setting interval ≤ 0 must
// disable dreaming entirely. This is the documented kill-switch for
// users who don't want background API calls.
func TestDreamGates_DisabledReturnsFalse(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "0")
	d := shouldFireDream(time.Time{}, time.Time{}, t.TempDir(), time.Now())
	if d.Fire {
		t.Fatalf("interval=0 should disable dreaming, got Fire=true (reason=%s)", d.Reason)
	}
	if d.Reason != "disabled" {
		t.Fatalf("expected reason=disabled, got %s", d.Reason)
	}
}

// TestDreamGates_TooSoon — when lastSuccessAt is more recent than the
// interval, the time gate must reject without doing any I/O. We pass
// a non-existent sessionsDir to prove no directory scan happens.
func TestDreamGates_TooSoon(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "6")
	now := time.Now()
	d := shouldFireDream(
		now.Add(-3*time.Hour), // 3 h ago, well within the 6 h window
		time.Time{},
		"/non/existent/path",
		now,
	)
	if d.Fire {
		t.Fatalf("3h-ago lastSuccessAt should fail 6h gate, got Fire=true")
	}
	if d.Reason != "too-soon" {
		t.Fatalf("expected reason=too-soon, got %s", d.Reason)
	}
}

// TestDreamGates_Throttled — even if the time gate passes, a recent
// scan within DreamScanThrottle (10 min) must rebuff the dream. This
// caps directory-walk cost on long-idle conversations that resume
// abruptly.
func TestDreamGates_Throttled(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "6")
	now := time.Now()
	d := shouldFireDream(
		now.Add(-7*time.Hour),   // time gate passes
		now.Add(-2*time.Minute), // scanned 2 min ago — still throttled
		"/non/existent/path",
		now,
	)
	if d.Fire {
		t.Fatalf("2min-ago scan should be throttled, got Fire=true")
	}
	if d.Reason != "throttled" {
		t.Fatalf("expected reason=throttled, got %s", d.Reason)
	}
}

// TestDreamGates_Thin — when time + scan gates pass but the sessions
// dir doesn't have enough new material, the gate refuses with
// reason=thin.
func TestDreamGates_Thin(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "6")
	t.Setenv("METIS_DREAM_MIN_SESSIONS", "3")

	dir := t.TempDir()
	// Seed one session — below the threshold of 3.
	mustWriteSession(t, dir, "a.jsonl", time.Now())

	d := shouldFireDream(
		time.Now().Add(-7*time.Hour),
		time.Time{},
		dir,
		time.Now(),
	)
	if d.Fire {
		t.Fatalf("1 session < 3 threshold should refuse, got Fire=true")
	}
	if d.Reason != "thin" {
		t.Fatalf("expected reason=thin, got %s", d.Reason)
	}
}

// TestDreamGates_FireWhenAllPass — happy path: all three gates clear.
// Sets up a sessions dir with 5 fresh JSONLs, time gate passes, no
// recent scan.
func TestDreamGates_FireWhenAllPass(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "6")
	t.Setenv("METIS_DREAM_MIN_SESSIONS", "3")

	dir := t.TempDir()
	now := time.Now()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		mustWriteSession(t, dir, n+".jsonl", now)
	}

	d := shouldFireDream(
		now.Add(-7*time.Hour),
		time.Time{},
		dir,
		now,
	)
	if !d.Fire {
		t.Fatalf("all gates should pass with 5 sessions + 7h-old lock, got Fire=false (reason=%s)", d.Reason)
	}
	if d.Reason != "ok" {
		t.Fatalf("expected reason=ok, got %s", d.Reason)
	}
}

// TestDreamGates_FirstRunCountsAll — when lastSuccessAt is zero (fresh
// install, never dreamed), the session count should include every
// session file regardless of mtime. Without this carve-out the gate
// would refuse forever on a fresh install with old session files.
func TestDreamGates_FirstRunCountsAll(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "6")
	t.Setenv("METIS_DREAM_MIN_SESSIONS", "3")

	dir := t.TempDir()
	// Sessions are all 30+ days old — still count on first run.
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, n := range []string{"a", "b", "c", "d"} {
		mustWriteSession(t, dir, n+".jsonl", old)
	}

	d := shouldFireDream(
		time.Time{}, // never dreamed
		time.Time{},
		dir,
		time.Now(),
	)
	if !d.Fire {
		t.Fatalf("fresh install + 4 old sessions should fire, got Fire=false (reason=%s)", d.Reason)
	}
}

// TestCountSessionsTouchedSince_MissingDir — a brand-new metis install
// hasn't created ~/.metis/sessions yet. The count function must
// return (0, nil), not propagate ENOENT — otherwise dreaming would
// fail-noisy on day 1.
func TestCountSessionsTouchedSince_MissingDir(t *testing.T) {
	n, err := countSessionsTouchedSince("/definitely/does/not/exist", time.Time{})
	if err != nil {
		t.Fatalf("missing dir should be silent, got err=%v", err)
	}
	if n != 0 {
		t.Fatalf("missing dir should report 0 sessions, got %d", n)
	}
}

// TestCountSessionsTouchedSince_IgnoresNonJSONL — only files ending in
// .jsonl count. tags/, exports/, and stray .DS_Store get filtered.
func TestCountSessionsTouchedSince_IgnoresNonJSONL(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mustWriteSession(t, dir, "real.jsonl", now)
	mustWriteSession(t, dir, ".DS_Store", now)
	mustWriteSession(t, dir, "notes.md", now)
	// Sub-dirs (tags/, exports/) — ReadDir entry IsDir() filter.
	if err := os.Mkdir(filepath.Join(dir, "tags"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	n, err := countSessionsTouchedSince(dir, time.Time{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("only real.jsonl should count, got %d", n)
	}
}

// mustWriteSession is a tiny helper to seed a .jsonl with a known
// mtime so tests are deterministic.
func mustWriteSession(t *testing.T, dir, name string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(`{"kind":"header"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", p, err)
	}
}
