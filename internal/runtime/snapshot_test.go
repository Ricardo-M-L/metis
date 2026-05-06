package runtime

import (
	"os"
	"testing"
	"time"
)

// withTempMetisHome redirects METIS_HOME at a t.TempDir() so test
// snapshots don't leak to the real ~/.metis. Restored on test exit.
func withTempMetisHome(t *testing.T) string {
	t.Helper()
	prev := os.Getenv("METIS_HOME")
	tmp := t.TempDir()
	os.Setenv("METIS_HOME", tmp)
	t.Cleanup(func() {
		if prev == "" {
			os.Unsetenv("METIS_HOME")
		} else {
			os.Setenv("METIS_HOME", prev)
		}
	})
	return tmp
}

func TestSaveLoadSnapshot_Roundtrip(t *testing.T) {
	withTempMetisHome(t)
	in := Snapshot{
		SessionID:      "abc123",
		StartedAt:      time.Now().Add(-1 * time.Hour),
		Cwd:            "/tmp/test",
		Provider:       "anthropic",
		Model:          "claude-haiku-4-5",
		MessageCount:   42,
		TokensTotal:    12345,
		Phase:          "streaming",
		LastUserPrompt: "refactor the eval package",
	}
	path, err := SaveSnapshot(in)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if path == "" {
		t.Fatal("save returned empty path")
	}
	out, err := LoadSnapshot("abc123")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.SessionID != in.SessionID || out.Cwd != in.Cwd || out.Model != in.Model {
		t.Errorf("roundtrip mismatch: in=%+v out=%+v", in, out)
	}
	if out.SchemaVersion != SnapshotSchemaVersion {
		t.Errorf("schema version should be auto-stamped; got %d", out.SchemaVersion)
	}
	if out.PID == 0 {
		t.Error("PID should be auto-stamped to os.Getpid()")
	}
}

func TestSaveSnapshot_RequiresSessionID(t *testing.T) {
	withTempMetisHome(t)
	if _, err := SaveSnapshot(Snapshot{}); err == nil {
		t.Error("save with empty SessionID must error")
	}
}

func TestLoadSnapshot_MissingReturnsErrNotExist(t *testing.T) {
	withTempMetisHome(t)
	_, err := LoadSnapshot("never-existed")
	if !os.IsNotExist(err) {
		t.Errorf("missing snapshot should return os.ErrNotExist; got %v", err)
	}
}

func TestListSnapshots_OrdersByUpdatedAtDesc(t *testing.T) {
	withTempMetisHome(t)
	now := time.Now()
	for i, age := range []time.Duration{2 * time.Hour, 5 * time.Minute, 1 * time.Hour} {
		s := Snapshot{
			SessionID: []string{"old", "newest", "middle"}[i],
			UpdatedAt: now.Add(-age),
			Cwd:       "/tmp",
		}
		if _, err := SaveSnapshot(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots; got %d", len(got))
	}
	wantOrder := []string{"newest", "middle", "old"}
	for i, w := range wantOrder {
		if got[i].SessionID != w {
			t.Errorf("[%d] expected %q; got %q", i, w, got[i].SessionID)
		}
	}
}

func TestDeleteSnapshot_Idempotent(t *testing.T) {
	withTempMetisHome(t)
	if _, err := SaveSnapshot(Snapshot{SessionID: "ditch"}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSnapshot("ditch"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := DeleteSnapshot("ditch"); err != nil {
		t.Errorf("second delete (already gone) should not error; got %v", err)
	}
	if err := DeleteSnapshot("never-existed"); err != nil {
		t.Errorf("delete of nonexistent snapshot should not error; got %v", err)
	}
}

func TestLooksCrashed_LiveProcessNotCrashed(t *testing.T) {
	s := Snapshot{
		SessionID: "live",
		PID:       os.Getpid(),
		UpdatedAt: time.Now(),
	}
	if LooksCrashed(s, 0) {
		t.Error("snapshot from THIS process must never look crashed")
	}
}

func TestLooksCrashed_DeadProcess(t *testing.T) {
	// PID 1 (init) is always running on linux/darwin. To force "not
	// running," use a pid that's almost certainly not in use — a
	// negative would short-circuit, so use a very high one. signal 0
	// to a non-existent pid returns ESRCH.
	deadPID := 999999
	s := Snapshot{
		SessionID: "dead",
		PID:       deadPID,
		UpdatedAt: time.Now(),
	}
	if !LooksCrashed(s, 0) {
		t.Error("snapshot owned by a non-existent pid should look crashed")
	}
}

func TestLooksCrashed_StaleEvenIfPidExists(t *testing.T) {
	// Live pid (this process) but UpdatedAt very old + maxAge tight
	// → not classified as crashed, because pid match short-circuits.
	// This tests the priority order documented in the function.
	s := Snapshot{
		SessionID: "stale-but-mine",
		PID:       os.Getpid(),
		UpdatedAt: time.Now().Add(-100 * time.Hour),
	}
	if LooksCrashed(s, 1*time.Hour) {
		t.Error("self-pid wins over stale UpdatedAt — not crashed")
	}
}

func TestSnapshotPath_RejectsTraversal(t *testing.T) {
	withTempMetisHome(t)
	bad := []string{
		"../etc",
		`..\windows`,
		"a/b",
		`c\d`,
		"x/../../y",
		"..",
		"foo/../bar",
	}
	for _, sid := range bad {
		if _, err := SaveSnapshot(Snapshot{SessionID: sid}); err == nil {
			t.Errorf("SaveSnapshot(%q) should reject path-traversing id", sid)
		}
	}
}

func TestSnapshotPath_AcceptsCleanIDs(t *testing.T) {
	withTempMetisHome(t)
	good := []string{
		"abc123",
		"01HFG3K8M9R7B0V2N3P4Q5T6X7", // ULID-shaped
		"sess-2026-05-06-abc",
		"x.y.z", // dots OK as long as not ".."
	}
	for _, sid := range good {
		if _, err := SaveSnapshot(Snapshot{SessionID: sid}); err != nil {
			t.Errorf("SaveSnapshot(%q) should accept clean id; got %v", sid, err)
		}
	}
}

func TestProcessExists_Self(t *testing.T) {
	if !processExists(os.Getpid()) {
		t.Error("processExists must return true for our own pid")
	}
	if processExists(0) {
		t.Error("processExists(0) must return false (invalid pid)")
	}
	if processExists(-1) {
		t.Error("processExists(-1) must return false (invalid pid)")
	}
}
