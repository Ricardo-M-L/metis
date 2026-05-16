package memory

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestDreamLock_AcquireReleaseRoundtrip — happy path: TryAcquire on a
// fresh memdir succeeds, lock file contains our PID after write, and
// LastSuccessAt advances to "around now" after Release.
func TestDreamLock_AcquireReleaseRoundtrip(t *testing.T) {
	dir := t.TempDir()
	l := NewDreamLock(dir)

	if !l.LastSuccessAt().IsZero() {
		t.Fatalf("fresh memdir should report zero LastSuccessAt, got %v", l.LastSuccessAt())
	}

	prior, ok, err := l.TryAcquire()
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !ok {
		t.Fatalf("TryAcquire should succeed on empty memdir")
	}
	if !prior.IsZero() {
		t.Fatalf("first acquire should report zero priorMtime, got %v", prior)
	}

	// File should now contain our PID.
	body, err := os.ReadFile(filepath.Join(dir, DreamLockFilename))
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if string(body) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock body = %q, want PID %d", string(body), os.Getpid())
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// After Release, body should be empty (cleanly completed) but mtime
	// should still be recent.
	body2, err := os.ReadFile(filepath.Join(dir, DreamLockFilename))
	if err != nil {
		t.Fatalf("read after release: %v", err)
	}
	if len(body2) != 0 {
		t.Fatalf("after Release, lock body should be empty, got %q", string(body2))
	}
	if time.Since(l.LastSuccessAt()) > 5*time.Second {
		t.Fatalf("LastSuccessAt should be within 5s of now, got %v", l.LastSuccessAt())
	}
}

// TestDreamLock_RollbackRestoresMtime — Rollback must put the mtime
// back to priorMtime so a failed dream doesn't reset the 6 h timer.
// Without this, every crash would lock subsequent processes out for
// the full interval.
func TestDreamLock_RollbackRestoresMtime(t *testing.T) {
	dir := t.TempDir()
	l := NewDreamLock(dir)

	// Pre-seed a lock at a known mtime ("last dream was 3 days ago").
	// Body must be empty — that's the post-Release state, the only
	// shape LastSuccessAt treats as "completed cleanly". A non-empty
	// body would trigger the new stale-PID detection and return zero.
	threeDaysAgo := time.Now().Add(-3 * 24 * time.Hour)
	if err := os.WriteFile(l.Path(), []byte{}, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chtimes(l.Path(), threeDaysAgo, threeDaysAgo); err != nil {
		t.Fatalf("chtimes seed: %v", err)
	}

	prior, ok, err := l.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("TryAcquire on seeded lock: ok=%v err=%v", ok, err)
	}
	// priorMtime should match what we seeded (within FS resolution).
	if delta := prior.Sub(threeDaysAgo).Abs(); delta > time.Second {
		t.Fatalf("priorMtime %v not within 1s of seed %v (Δ=%v)", prior, threeDaysAgo, delta)
	}

	// Simulate a dream failure — roll back.
	if err := l.Rollback(prior); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if delta := l.LastSuccessAt().Sub(threeDaysAgo).Abs(); delta > time.Second {
		t.Fatalf("after Rollback, LastSuccessAt %v not within 1s of seed %v (Δ=%v)",
			l.LastSuccessAt(), threeDaysAgo, delta)
	}
}

// TestDreamLock_RollbackFromZero — when the lock didn't exist before
// the failed acquire, Rollback must remove the file so the next
// caller's LastSuccessAt reports zero (= "never dreamed").
func TestDreamLock_RollbackFromZero(t *testing.T) {
	dir := t.TempDir()
	l := NewDreamLock(dir)

	prior, ok, err := l.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("TryAcquire: ok=%v err=%v", ok, err)
	}
	if !prior.IsZero() {
		t.Fatalf("expected zero priorMtime on fresh memdir")
	}
	if err := l.Rollback(prior); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Fatalf("Rollback from zero should remove lock file, stat err=%v", err)
	}
	if !l.LastSuccessAt().IsZero() {
		t.Fatalf("after Rollback-from-zero, LastSuccessAt should be zero")
	}
}

// TestDreamLock_StaleCrashedLockTreatedAsExpired — when a previous
// dream crashed (process killed mid-fork, leaving a non-empty PID
// body and a recent mtime), LastSuccessAt must NOT treat it as a
// recent successful completion. Otherwise every subsequent process
// sees the stale lock and refuses to dream for 6 h. Returning zero
// lets the next dream proceed.
func TestDreamLock_StaleCrashedLockTreatedAsExpired(t *testing.T) {
	dir := t.TempDir()
	l := NewDreamLock(dir)

	// Seed a lock with a definitely-dead PID and a recent mtime.
	// PID 1 is init/launchd on Unix — never dies — so that won't
	// work. We use a synthetic huge PID that won't be allocated.
	if err := os.WriteFile(l.Path(), []byte("9999999"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now := time.Now()
	if err := os.Chtimes(l.Path(), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := l.LastSuccessAt()
	if !got.IsZero() {
		t.Fatalf("stale-PID lock should report zero LastSuccessAt, got %v (would block dreams for 6h)", got)
	}
}

// TestDreamLock_LivePIDLockTreatedAsInflight — when the body contains
// the CURRENT process's PID, LastSuccessAt returns the mtime so the
// time gate refuses (we don't want one process double-firing while
// its own dream is in flight). Pairs with the stale-crashed case.
func TestDreamLock_LivePIDLockTreatedAsInflight(t *testing.T) {
	dir := t.TempDir()
	l := NewDreamLock(dir)

	// Use our own PID — guaranteed alive by virtue of running this test.
	if err := os.WriteFile(l.Path(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now := time.Now()
	if err := os.Chtimes(l.Path(), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := l.LastSuccessAt()
	if got.IsZero() {
		t.Fatalf("live-PID lock should report mtime (gate refuses), got zero")
	}
}

// TestDreamLock_ConcurrentAcquire — two locks pointing at the same
// memdir both TryAcquire; exactly one should win. The losing acquire
// must return ok=false, err=nil so the caller can bail cleanly.
//
// Sequential rather than parallel to keep the test deterministic: the
// second acquire happens after the first finishes writing but the
// re-read sees the second's PID, not the first's — so the first call
// returns ok=false. (Real two-process race: kernel scheduling decides
// the winner. We exercise the same code path.)
func TestDreamLock_ConcurrentAcquire(t *testing.T) {
	dir := t.TempDir()
	l1 := NewDreamLock(dir)
	l2 := NewDreamLock(dir)

	// l1 writes its PID, sleeps inside TryAcquire (20ms settle window),
	// then re-reads. We race a competing write that overwrites l1's
	// PID before l1 verifies. The settle window must be big enough that
	// under CI load (busy GitHub-Actions runners) we reliably get the
	// competing write in BEFORE the verifier reads.
	done := make(chan struct{})
	var l1ok, l2ok bool
	var l1err, l2err error
	go func() {
		_, l1ok, l1err = l1.TryAcquire()
		close(done)
	}()
	// Inject the competing write ~halfway into the 20ms settle window —
	// far enough after l1's WriteFile to ensure it happened first, far
	// enough before re-read to be visible. "1" is a fake PID clearly
	// different from os.Getpid().
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(l1.Path(), []byte("1"), 0o644); err != nil {
		t.Fatalf("race-write: %v", err)
	}
	<-done
	if l1err != nil {
		t.Fatalf("l1.TryAcquire err: %v", l1err)
	}
	if l1ok {
		t.Fatalf("l1 should have lost the race (its PID was overwritten)")
	}

	// l2 acquires cleanly on a fresh ground.
	_, l2ok, l2err = l2.TryAcquire()
	if l2err != nil || !l2ok {
		t.Fatalf("l2 follow-up acquire: ok=%v err=%v", l2ok, l2err)
	}
}
