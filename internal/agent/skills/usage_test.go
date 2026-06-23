package skills

import (
	"testing"
	"time"
)

func TestUsageStore_RecordsAndProvenance(t *testing.T) {
	dir := t.TempDir()
	u := NewUsageStore(dir)

	u.RecordCreate("foo", SourceAgentSynth)
	u.RecordUse("foo")
	u.RecordUse("foo")
	u.RecordView("foo")
	u.RecordPatch("foo")

	rec, ok := u.Get("foo")
	if !ok {
		t.Fatal("expected a record")
	}
	if rec.UseCount != 2 || rec.ViewCount != 1 || rec.PatchCount != 1 {
		t.Errorf("counts: use=%d view=%d patch=%d", rec.UseCount, rec.ViewCount, rec.PatchCount)
	}
	if rec.ActivityCount() != 4 {
		t.Errorf("ActivityCount=%d want 4", rec.ActivityCount())
	}
	if rec.Source != SourceAgentSynth || rec.CreatedAt == "" {
		t.Errorf("provenance not stamped: %+v", rec)
	}
	if !u.IsAgentCreated("foo") {
		t.Error("IsAgentCreated should be true for a synth-created skill")
	}
	if u.IsAgentCreated("never-seen") {
		t.Error("IsAgentCreated should be false for an unknown skill")
	}
	// Persisted across store instances (same dir).
	if r2, ok := NewUsageStore(dir).Get("foo"); !ok || r2.UseCount != 2 {
		t.Errorf("usage not persisted: ok=%v rec=%+v", ok, r2)
	}
}

func TestUsageStore_NoopOnEmptyDir(t *testing.T) {
	u := NewUsageStore("") // must not panic / write anywhere
	u.RecordUse("x")
	if _, ok := u.Get("x"); ok {
		t.Error("empty-dir store should record nothing")
	}
}

func TestDeriveState(t *testing.T) {
	now := time.Now()
	idleAfter := 21 * 24 * time.Hour
	archiveAfter := 90 * 24 * time.Hour
	mk := func(daysAgo int) UsageRecord {
		return UsageRecord{LastUsedAt: now.AddDate(0, 0, -daysAgo).UTC().Format(time.RFC3339)}
	}
	cases := []struct {
		days int
		want SkillState
	}{
		{1, StateActive},
		{30, StateIdle},
		{120, StateArchived},
	}
	for _, c := range cases {
		if got := DeriveState(mk(c.days), now, idleAfter, archiveAfter); got != c.want {
			t.Errorf("DeriveState(%dd)=%s want %s", c.days, got, c.want)
		}
	}
	// No timestamps → treated as freshly active, never archived.
	if got := DeriveState(UsageRecord{}, now, idleAfter, archiveAfter); got != StateActive {
		t.Errorf("empty record → %s want active", got)
	}
}

func TestCurator_UsageActivityKeepsSkillFresh(t *testing.T) {
	root := t.TempDir()
	// File mtime is ancient (would normally archive), but a recent usage
	// record must keep it out of the archive set.
	writeSkillFile(t, root, "still-used", 200)
	NewUsageStore(root).RecordUse("still-used") // last_used = now

	res, err := NewCurator(root).Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("a recently-used skill must not be archived despite old mtime, got %v", res.Archived)
	}
	if res.Skipped != 1 {
		t.Errorf("want Skipped=1 (kept fresh by usage), got %d", res.Skipped)
	}
}
