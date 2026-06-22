package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSkillFile drops a flat skill .md with a mtime `ageDays` in the past.
func writeSkillFile(t *testing.T, root, name string, ageDays int) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, name+".md")
	body := "---\nname: " + name + "\ndescription: test skill\n---\nbody\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().AddDate(0, 0, -ageDays)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestCurator_ArchivesIdleNotFresh(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "stale-skill", 200) // older than 90d → archive
	writeSkillFile(t, root, "fresh-skill", 3)   // recent → keep

	c := NewCurator(root)
	res, err := c.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "stale-skill" {
		t.Fatalf("want [stale-skill] archived, got %v", res.Archived)
	}
	// stale moved out of root, fresh stayed.
	if _, err := os.Stat(filepath.Join(root, "stale-skill.md")); !os.IsNotExist(err) {
		t.Error("stale-skill should be gone from root")
	}
	if _, err := os.Stat(filepath.Join(root, "fresh-skill.md")); err != nil {
		t.Error("fresh-skill should remain in root")
	}
	// stale recoverable from .archive.
	if _, err := os.Stat(filepath.Join(root, curatorArchiveDir, "stale-skill.md")); err != nil {
		t.Error("stale-skill should be recoverable in .archive/")
	}
}

func TestCurator_PinProtects(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "load-bearing", 365)
	c := NewCurator(root)
	if err := c.Pin("load-bearing"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("pinned skill must not be archived, got %v", res.Archived)
	}
	if res.Pinned != 1 {
		t.Errorf("want Pinned=1, got %d", res.Pinned)
	}
	if _, err := os.Stat(filepath.Join(root, "load-bearing.md")); err != nil {
		t.Error("pinned skill should stay in root")
	}
}

func TestCurator_SkipsInstalledDirSkills(t *testing.T) {
	root := t.TempDir()
	// Installed marketplace skill = directory with SKILL.md — must be
	// off-limits to the curator even if ancient.
	dir := filepath.Join(root, "docx")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sk := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(sk, []byte("---\nname: docx\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -400)
	_ = os.Chtimes(sk, old, old)
	_ = os.Chtimes(dir, old, old)

	res, err := NewCurator(root).Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("installed dir skill must not be archived, got %v", res.Archived)
	}
	if _, err := os.Stat(sk); err != nil {
		t.Error("installed dir skill should be untouched")
	}
}

func TestCurator_RestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "one-off", 120)
	c := NewCurator(root)
	if _, err := c.Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}
	archived, err := c.ListArchived()
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0] != "one-off" {
		t.Fatalf("want [one-off] archived, got %v", archived)
	}
	if err := c.Restore("one-off"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "one-off.md")); err != nil {
		t.Error("restored skill should be back in root")
	}
	// Restore refuses to clobber a live skill of the same name.
	writeSkillFile(t, root, "dup", 120)
	if _, err := c.Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, root, "dup", 1) // recreate live
	if err := c.Restore("dup"); err == nil {
		t.Error("Restore should refuse to overwrite a live skill")
	}
}

func TestCurator_IdleCandidatesIsDryRun(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "stale", 200)
	writeSkillFile(t, root, "fresh", 1)
	c := NewCurator(root)
	cands, err := c.IdleCandidates(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0] != "stale" {
		t.Fatalf("want [stale], got %v", cands)
	}
	// Dry-run must not have moved anything.
	if _, err := os.Stat(filepath.Join(root, "stale.md")); err != nil {
		t.Error("IdleCandidates must not archive — stale.md should still be in root")
	}
}

func TestCurator_CustomIdleWindow(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "week-old", 8)
	// Default 90d: not archived. With 7d window: archived.
	if res, _ := NewCurator(root).Sweep(time.Now()); len(res.Archived) != 0 {
		t.Fatalf("default window should keep an 8-day skill, got %v", res.Archived)
	}
	res, err := NewCurator(root).WithIdleDays(7).Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 {
		t.Fatalf("7-day window should archive an 8-day skill, got %v", res.Archived)
	}
}

func TestCurator_RejectsPathTraversalNames(t *testing.T) {
	root := t.TempDir()
	c := NewCurator(root)
	for _, bad := range []string{"../evil", "../../etc/passwd", "a/b", `..\evil`, "", ".", ".."} {
		if err := c.Restore(bad); err == nil {
			t.Errorf("Restore(%q) should be rejected", bad)
		}
		if err := c.Pin(bad); err == nil {
			t.Errorf("Pin(%q) should be rejected", bad)
		}
		if err := c.Unpin(bad); err == nil {
			t.Errorf("Unpin(%q) should be rejected", bad)
		}
	}
}

func TestCurator_SkipsSymlinkedSkills(t *testing.T) {
	root := t.TempDir()
	// A team-shared skill symlinked into the root must not be archived
	// (archiving would move/break the link).
	target := filepath.Join(t.TempDir(), "shared.md")
	if err := os.WriteFile(target, []byte("---\nname: shared\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shared.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	old := time.Now().AddDate(0, 0, -400)
	_ = os.Chtimes(target, old, old)

	res, err := NewCurator(root).Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("symlinked skill must not be archived, got %v", res.Archived)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("symlink should be left intact")
	}
}

func TestCurator_RestoreResetsIdleClock(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "one-off", 200)
	c := NewCurator(root)
	if _, err := c.Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore("one-off"); err != nil {
		t.Fatal(err)
	}
	// The restored skill must NOT be re-archived on the very next sweep —
	// Restore reset its mtime to now, so it gets a full fresh idle window.
	res, err := c.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("restored skill must survive the next sweep, got re-archived: %v", res.Archived)
	}
}

func TestCurator_CorruptPinsFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, root, "stale", 200)
	// Corrupt the pins file.
	if err := os.WriteFile(filepath.Join(root, curatorPinsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCurator(root)
	// Sweep must fail (fail-safe: never archive when pin state is
	// unreadable) rather than silently treating pins as empty and
	// archiving a possibly-pinned skill.
	if _, err := c.Sweep(time.Now()); err == nil {
		t.Error("Sweep should error on a corrupt pins file, not archive")
	}
	// stale must NOT have been archived.
	if _, err := os.Stat(filepath.Join(root, "stale.md")); err != nil {
		t.Error("stale.md must remain (sweep aborted), not be archived")
	}
}

func TestCurator_CaseInsensitiveExtension(t *testing.T) {
	root := t.TempDir()
	// Hand-created uppercase extension: scan must see it AND archive must
	// move it (a prior version scanned it but archive re-derived a
	// lowercase path and failed forever).
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "Loud.MD")
	if err := os.WriteFile(p, []byte("---\nname: Loud\n---\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -200)
	_ = os.Chtimes(p, old, old)

	c := NewCurator(root)
	res, err := c.Sweep(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "Loud" || res.Failed != 0 {
		t.Fatalf("Loud.MD should archive cleanly, got archived=%v failed=%d", res.Archived, res.Failed)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("Loud.MD should be gone from root")
	}
	// And it round-trips on restore (original extension casing preserved).
	if err := c.Restore("Loud"); err != nil {
		t.Fatalf("restore Loud: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("restored Loud.MD should be back in root with original name")
	}
}

func TestCurator_ArchiveInvisibleToLoader(t *testing.T) {
	// The whole point: an archived skill must not re-surface in the skill
	// list. dirLayer skips dot-prefixed entries, so .archive/ (and the pins
	// file) are invisible. Guard that end-to-end through the loader's scan.
	root := t.TempDir()
	writeSkillFile(t, root, "live-skill", 1)
	writeSkillFile(t, root, "retired", 200)
	c := NewCurator(root)
	if _, err := c.Sweep(time.Now()); err != nil { // moves retired → .archive/
		t.Fatal(err)
	}
	if err := c.Pin("live-skill"); err != nil { // writes .curator-pins.json
		t.Fatal(err)
	}

	got, err := dirLayer("test", 1, root, "").Scan()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if names["retired"] {
		t.Error("archived skill 'retired' leaked back into the loader's scan")
	}
	if !names["live-skill"] {
		t.Error("live-skill should still be visible to the loader")
	}
	// The pins json must not parse as a phantom skill.
	if len(got) != 1 {
		t.Errorf("expected exactly 1 visible skill (live-skill), got %d: %v", len(got), names)
	}
}
