package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSynth_CreateSkill_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)

	path, err := s.CreateSkill(SynthSkillMeta{
		Name:        "test-skill",
		Description: "a synthetic skill",
		WhenToUse:   "when running tests",
	}, "# Body\n\nDo X then Y.")
	if err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	if !strings.HasSuffix(path, "test-skill.md") {
		t.Fatalf("unexpected path %q", path)
	}

	// Loader can read what Synth wrote.
	sk, err := Load(path)
	if err != nil {
		t.Fatalf("Load round-trip: %v", err)
	}
	if sk.Name != "test-skill" {
		t.Errorf("name = %q, want test-skill", sk.Name)
	}
	if sk.Description != "a synthetic skill" {
		t.Errorf("description = %q, want 'a synthetic skill'", sk.Description)
	}
	if sk.WhenToUse != "when running tests" {
		t.Errorf("when_to_use = %q", sk.WhenToUse)
	}
	if !strings.Contains(sk.Prompt, "Do X then Y") {
		t.Errorf("body lost: prompt = %q", sk.Prompt)
	}
	if sk.CreatedAt == "" {
		t.Errorf("CreatedAt should be auto-stamped, got empty")
	}
}

func TestSynth_CreateSkill_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	_, err := s.CreateSkill(SynthSkillMeta{Name: "dup", Description: "first"}, "body1")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err = s.CreateSkill(SynthSkillMeta{Name: "dup", Description: "second"}, "body2")
	if err == nil {
		t.Fatalf("second Create on same name should refuse")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
}

func TestSynth_CreateSkill_RejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	cases := []string{
		"",
		"../traversal",
		"with spaces",
		"with/slash",
		".hidden",
		"UPPER_AND_lower",        // gets lowercased but check post-clean still alnum
		strings.Repeat("x", 100), // too long
		"a",                      // too short (min 2 chars)
	}
	for _, c := range cases {
		_, err := s.CreateSkill(SynthSkillMeta{Name: c, Description: "x"}, "body")
		// Lowercasing alone could rescue "UPPER_AND_lower" — verify
		// it was either accepted normalized or properly refused.
		if c == "UPPER_AND_lower" {
			if err != nil && !strings.Contains(err.Error(), "invalid") {
				t.Errorf("name %q: unexpected error %v", c, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("name %q should be rejected, got nil err", c)
		}
	}
}

func TestSynth_UpdateSkill_KeepsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	_, err := s.CreateSkill(SynthSkillMeta{
		Name:        "git-rebase",
		Description: "interactive rebase helper",
		WhenToUse:   "tidying history",
	}, "# Original body")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = s.UpdateSkill("git-rebase", "# Revised body\n\nNew steps.")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	sk, err := Load(filepath.Join(dir, "git-rebase.md"))
	if err != nil {
		t.Fatalf("Load after update: %v", err)
	}
	// Frontmatter survives.
	if sk.Description != "interactive rebase helper" {
		t.Errorf("description lost: %q", sk.Description)
	}
	if sk.WhenToUse != "tidying history" {
		t.Errorf("when_to_use lost: %q", sk.WhenToUse)
	}
	// Body is replaced.
	if strings.Contains(sk.Prompt, "Original") {
		t.Errorf("old body still present: %q", sk.Prompt)
	}
	if !strings.Contains(sk.Prompt, "Revised body") {
		t.Errorf("new body missing: %q", sk.Prompt)
	}
}

func TestSynth_UpdateSkill_RefusesMissing(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	_, err := s.UpdateSkill("does-not-exist", "body")
	if err == nil {
		t.Fatalf("update on missing skill should refuse")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should mention 'does not exist': %v", err)
	}
}

func TestSynth_ListUserSkills(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	if got := s.ListUserSkills(); len(got) != 0 {
		t.Fatalf("empty dir should list nothing, got %v", got)
	}

	for _, n := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.CreateSkill(SynthSkillMeta{Name: n, Description: "x"}, "body"); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	// Decoys that must be filtered out.
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{}, 0o644); err != nil {
		t.Fatalf("decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte{}, 0o644); err != nil {
		t.Fatalf("decoy: %v", err)
	}

	got := s.ListUserSkills()
	if len(got) != 3 {
		t.Fatalf("expected 3 .md skills, got %d: %v", len(got), got)
	}
}

func TestSynth_LoaderInvalidatedOnWrite(t *testing.T) {
	dir := t.TempDir()
	// Use a real Loader pointed at a user-dir; Synth should call
	// Invalidate() so the next All() picks up our write.
	l := NewLoader(dir, "", nil)
	s := NewSynth(dir, l)

	if got := listOrFatal(t, l); !containsName(got, "freshly-synthesized") {
		// Sanity: not there yet.
	}

	if _, err := s.CreateSkill(SynthSkillMeta{Name: "freshly-synthesized", Description: "x"}, "body"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Loader should now see it.
	if got := listOrFatal(t, l); !containsName(got, "freshly-synthesized") {
		t.Fatalf("Loader.All() didn't pick up freshly-synthesized: %v", skillNames(got))
	}
}

func listOrFatal(t *testing.T, l *Loader) []Skill {
	t.Helper()
	out, err := l.List()
	if err != nil {
		t.Fatalf("Loader.List: %v", err)
	}
	return out
}

func containsName(skills []Skill, name string) bool {
	for _, sk := range skills {
		if sk.Name == name {
			return true
		}
	}
	return false
}

func skillNames(skills []Skill) []string {
	out := make([]string, 0, len(skills))
	for _, sk := range skills {
		out = append(out, sk.Name)
	}
	return out
}
