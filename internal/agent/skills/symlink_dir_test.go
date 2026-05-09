package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoader_FollowsSymlinkedDirAtTop covers the user-shared-skills
// case: a sysadmin sets `~/.metis/skills/team` as a symlink to
// `/opt/team-skills/`. Before the symlink-aware fix, ReadDir returned
// the link as a non-dir entry and the recursive scan skipped it; the
// shared skills were silently invisible.
//
// Builds two trees:
//
//	tmpUser/skills/
//	  team -> /tmpReal/team-skills/
//
//	tmpReal/team-skills/
//	  greppy/SKILL.md     ("name: greppy")
//
// And verifies the loader emits "greppy".
func TestLoader_FollowsSymlinkedDirAtTop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated perms on Windows; covered on Unix")
	}
	user := t.TempDir()
	real := t.TempDir()

	// /real/team-skills/greppy/SKILL.md
	greppyDir := filepath.Join(real, "team-skills", "greppy")
	if err := os.MkdirAll(greppyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`---
name: greppy
description: shared team grep helper
---
do grep things
`)
	if err := os.WriteFile(filepath.Join(greppyDir, "SKILL.md"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	// user/skills/team → /real/team-skills
	if err := os.Symlink(filepath.Join(real, "team-skills"), filepath.Join(user, "team")); err != nil {
		t.Fatal(err)
	}

	l := NewLoader(user, "", nil)
	skills, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sk := range skills {
		if sk.Name == "greppy" && sk.Description == "shared team grep helper" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(skills))
		for _, sk := range skills {
			names = append(names, sk.Name)
		}
		t.Errorf("greppy not found via symlinked dir; saw: %v", names)
	}
}

// TestLoader_FollowsSymlinkedSkillDir covers the per-skill case: the
// user has `~/.metis/skills/myskill` as a symlink directly to a
// project's checked-in skill dir. The link itself is the skill dir.
func TestLoader_FollowsSymlinkedSkillDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated perms on Windows")
	}
	user := t.TempDir()
	real := t.TempDir()

	// /real/myskill-src/SKILL.md
	src := filepath.Join(real, "myskill-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(`---
name: linked-skill
description: linked from project
---
linked body
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// user/skills/linked-skill → /real/myskill-src
	if err := os.Symlink(src, filepath.Join(user, "linked-skill")); err != nil {
		t.Fatal(err)
	}

	l := NewLoader(user, "", nil)
	skills, _ := l.List()
	found := false
	for _, sk := range skills {
		if sk.Name == "linked-skill" && sk.Description == "linked from project" {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(skills))
		for _, sk := range skills {
			names = append(names, sk.Name)
		}
		t.Errorf("linked-skill not loaded; saw: %v", names)
	}
}

// TestLoader_BrokenSymlinkSilentlyIgnored — a dangling symlink in
// the skills dir must not crash the loader nor surface as a missing
// dependency error.
func TestLoader_BrokenSymlinkSilentlyIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated perms on Windows")
	}
	user := t.TempDir()
	if err := os.Symlink(filepath.Join(user, "does-not-exist"), filepath.Join(user, "broken")); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(user, "", nil)
	if _, err := l.List(); err != nil {
		t.Errorf("broken symlink should not error: %v", err)
	}
}

func TestIsDirOrSymlinkToDir_RegularFile(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() == "f.txt" && isDirOrSymlinkToDir(e, regular) {
			t.Error("regular file should not be reported as dir-like")
		}
	}
}
