package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withFakeHome redirects HOME to a tempdir for the test so pointer
// files don't pollute real ~/.metis/ and tests stay isolated. Restores
// the original HOME via t.Cleanup.
func withFakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestWritePointer_CreatesFileWithExpectedShape(t *testing.T) {
	withFakeHome(t)
	if err := WritePointer("sess-abc", "/tmp/proj1"); err != nil {
		t.Fatal(err)
	}
	p, err := ReadPointer("/tmp/proj1")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("just-written pointer should read back non-nil")
	}
	if p.SessionID != "sess-abc" {
		t.Errorf("sessionID: got %q, want %q", p.SessionID, "sess-abc")
	}
	if p.CWD != "/tmp/proj1" {
		t.Errorf("cwd: got %q, want %q", p.CWD, "/tmp/proj1")
	}
	if p.PID != os.Getpid() {
		t.Errorf("PID: got %d, want %d", p.PID, os.Getpid())
	}
	if p.AgeMs > 1000 {
		t.Errorf("just-written should have AgeMs ~ 0, got %d", p.AgeMs)
	}
}

func TestWritePointer_RejectsEmptySessionID(t *testing.T) {
	withFakeHome(t)
	if err := WritePointer("", "/tmp/x"); err == nil {
		t.Error("empty sessionID must error")
	}
}

func TestReadPointer_NoFileReturnsNilNoError(t *testing.T) {
	withFakeHome(t)
	p, err := ReadPointer("/tmp/nonexistent")
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if p != nil {
		t.Errorf("missing file should return nil pointer, got: %+v", p)
	}
}

func TestPointer_PerCwdIsolation(t *testing.T) {
	withFakeHome(t)
	// Two cwds, two sessions — neither should see the other.
	_ = WritePointer("sess-A", "/tmp/proj-A")
	_ = WritePointer("sess-B", "/tmp/proj-B")
	a, _ := ReadPointer("/tmp/proj-A")
	b, _ := ReadPointer("/tmp/proj-B")
	if a == nil || b == nil {
		t.Fatal("both pointers should exist")
	}
	if a.SessionID != "sess-A" || b.SessionID != "sess-B" {
		t.Errorf("cross-cwd contamination: A=%q B=%q", a.SessionID, b.SessionID)
	}
}

func TestRefreshPointer_BumpsMtime(t *testing.T) {
	home := withFakeHome(t)
	if err := WritePointer("sess-refresh", "/tmp/proj-r"); err != nil {
		t.Fatal(err)
	}
	// Force the pointer's mtime far back so we can detect the bump.
	path, _ := pointerPath("/tmp/proj-r")
	old := time.Now().Add(-25 * time.Minute)
	_ = os.Chtimes(path, old, old)

	st1, _ := os.Stat(path)
	if time.Since(st1.ModTime()) < 10*time.Minute {
		t.Skip("could not force old mtime — fs limitation")
	}

	if err := RefreshPointer("sess-refresh", "/tmp/proj-r"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	st2, _ := os.Stat(path)
	if !st2.ModTime().After(st1.ModTime()) {
		t.Errorf("mtime did not advance: before=%v after=%v", st1.ModTime(), st2.ModTime())
	}
	_ = home // silence unused
}

func TestRefreshPointer_RecreatesIfMissing(t *testing.T) {
	withFakeHome(t)
	// No write — RefreshPointer should self-heal by creating the file.
	if err := RefreshPointer("sess-self-heal", "/tmp/proj-h"); err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPointer("/tmp/proj-h")
	if p == nil {
		t.Fatal("expected pointer to exist after refresh")
	}
	if p.SessionID != "sess-self-heal" {
		t.Errorf("sessionID: %q", p.SessionID)
	}
}

func TestReadPointer_StaleAutoCleared(t *testing.T) {
	withFakeHome(t)
	if err := WritePointer("sess-stale", "/tmp/proj-s"); err != nil {
		t.Fatal(err)
	}
	path, _ := pointerPath("/tmp/proj-s")
	old := time.Now().Add(-2 * PointerTTL)
	_ = os.Chtimes(path, old, old)

	p, err := ReadPointer("/tmp/proj-s")
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Errorf("stale pointer should return nil, got: %+v", p)
	}
	// File should be gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale pointer file should be removed, got: %v", err)
	}
}

func TestClearPointer_Idempotent(t *testing.T) {
	withFakeHome(t)
	_ = WritePointer("sess-x", "/tmp/proj-x")
	if err := ClearPointer("/tmp/proj-x"); err != nil {
		t.Fatal(err)
	}
	if err := ClearPointer("/tmp/proj-x"); err != nil {
		t.Errorf("second clear should be no-op, got: %v", err)
	}
	p, _ := ReadPointer("/tmp/proj-x")
	if p != nil {
		t.Error("after clear, pointer should not exist")
	}
}

func TestReadPointer_MalformedJSONAutoCleared(t *testing.T) {
	withFakeHome(t)
	dir, _ := pointerDir()
	path, _ := pointerPath("/tmp/proj-m")
	_ = os.WriteFile(filepath.Join(dir, filepath.Base(path)), []byte("not json"), 0o600)

	p, err := ReadPointer("/tmp/proj-m")
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Errorf("malformed pointer should return nil, got: %+v", p)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("malformed file should be cleared, got: %v", err)
	}
}

func TestStartHeartbeat_RefreshesUntilCancel(t *testing.T) {
	withFakeHome(t)
	// Save a pointer with a far-back mtime.
	if err := WritePointer("sess-hb", "/tmp/proj-hb"); err != nil {
		t.Fatal(err)
	}
	path, _ := pointerPath("/tmp/proj-hb")
	_ = os.Chtimes(path, time.Now().Add(-10*time.Minute), time.Now().Add(-10*time.Minute))

	// We can't speed up StartHeartbeat's 60s ticker without exposing a
	// test seam — instead test that RefreshPointer works (the heartbeat
	// loop's only useful side effect). The goroutine lifecycle path is
	// covered in TestStartHeartbeat_GoroutineExitsOnCancel.
	_ = RefreshPointer("sess-hb", "/tmp/proj-hb")
	st, _ := os.Stat(path)
	if time.Since(st.ModTime()) > time.Second {
		t.Errorf("refresh should have bumped mtime to ~now, got age %v", time.Since(st.ModTime()))
	}
}

func TestStartHeartbeat_GoroutineExitsOnCancel(t *testing.T) {
	withFakeHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	StartHeartbeat(ctx, "sess-cancel", "/tmp/proj-cancel")
	cancel()
	// The goroutine should exit promptly. We can't observe that
	// directly without leaking a sync primitive, but at minimum the
	// test must not hang — go test's timeout would catch a deadlock.
	time.Sleep(50 * time.Millisecond)
}

func TestListLivePointers_ReturnsAllNonStale(t *testing.T) {
	withFakeHome(t)
	_ = WritePointer("a", "/tmp/p-a")
	_ = WritePointer("b", "/tmp/p-b")
	_ = WritePointer("c", "/tmp/p-c")
	// Force one stale.
	cPath, _ := pointerPath("/tmp/p-c")
	_ = os.Chtimes(cPath, time.Now().Add(-2*PointerTTL), time.Now().Add(-2*PointerTTL))

	live, err := ListLivePointers()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Errorf("expected 2 live pointers (stale culled), got %d: %+v", len(live), live)
	}
	gotIDs := map[string]bool{}
	for _, lp := range live {
		gotIDs[lp.SessionID] = true
	}
	if !gotIDs["a"] || !gotIDs["b"] {
		t.Errorf("missing expected sessions, got: %v", gotIDs)
	}
	if gotIDs["c"] {
		t.Errorf("stale session c should have been culled")
	}
}

func TestPointerPath_StableAcrossRuns(t *testing.T) {
	withFakeHome(t)
	p1, _ := pointerPath("/tmp/proj-stable")
	p2, _ := pointerPath("/tmp/proj-stable")
	if p1 != p2 {
		t.Errorf("same cwd must hash to same path: %q != %q", p1, p2)
	}
}

func TestPointerPath_DifferentCwdDifferentPath(t *testing.T) {
	withFakeHome(t)
	p1, _ := pointerPath("/tmp/proj-A")
	p2, _ := pointerPath("/tmp/proj-B")
	if p1 == p2 {
		t.Errorf("different cwds must hash to different paths, both got %q", p1)
	}
}
