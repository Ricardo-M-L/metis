package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMD(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoader_UserLayer(t *testing.T) {
	user := t.TempDir()
	writeMD(t, user, "code-review", `---
name: code-review
description: User-defined review
---
do stuff
`)
	l := NewLoader(user, "", nil)
	skills, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	// Bundled placeholder + user layer ⇒ ≥ 2.
	found := false
	for _, sk := range skills {
		if sk.Name == "code-review" && sk.Description == "User-defined review" {
			found = true
		}
	}
	if !found {
		t.Errorf("user skill not found in: %+v", skills)
	}
}

func TestLoader_UserOverridesBundled(t *testing.T) {
	// User can override any bundled skill by writing a same-named .md.
	// Pick "code-review" because it's a stable, always-shipped builtin.
	user := t.TempDir()
	writeMD(t, user, "code-review", `---
name: code-review
description: I-overrode-it
---
my body
`)
	var logs []string
	l := NewLoader(user, "", nil)
	l.Logger = func(format string, args ...any) {
		logs = append(logs, format)
	}
	skills, _ := l.List()
	for _, sk := range skills {
		if sk.Name == "code-review" && sk.Description != "I-overrode-it" {
			t.Errorf("user override didn't win: %+v", sk)
		}
	}
	// Conflict should be logged.
	hasOverride := false
	for _, log := range logs {
		if strings.Contains(log, "overrides") {
			hasOverride = true
		}
	}
	if !hasOverride {
		t.Error("expected override log message")
	}
}

func TestLoader_ProjectOverridesUser(t *testing.T) {
	user := t.TempDir()
	project := t.TempDir()
	writeMD(t, user, "shared", `---
name: shared
description: from-user
---
u`)
	writeMD(t, project, "shared", `---
name: shared
description: from-project
---
p`)
	l := NewLoader(user, project, nil)
	got, _ := l.Get("shared")
	if got == nil || got.Description != "from-project" {
		t.Errorf("project should win; got %+v", got)
	}
}

func TestLoader_PluginLayer(t *testing.T) {
	plug := stubPlugin{
		name: "p1",
		skills: []Skill{
			{Name: "from-plugin", Description: "via plugin"},
		},
	}
	l := NewLoader("", "", []PluginSkillSource{plug})
	skills, _ := l.List()
	found := false
	for _, sk := range skills {
		if sk.Name == "from-plugin" {
			found = true
		}
	}
	if !found {
		t.Error("plugin-contributed skill missing from List")
	}
}

type stubPlugin struct {
	name   string
	skills []Skill
}

func (s stubPlugin) Name() string     { return s.name }
func (s stubPlugin) Skills() []Skill  { return s.skills }

func TestLoader_CacheUntilInvalidate(t *testing.T) {
	user := t.TempDir()
	writeMD(t, user, "alpha", "---\nname: alpha\n---\nx")
	l := NewLoader(user, "", nil)

	got1, _ := l.List()
	// Add a new skill on disk; without Invalidate, List shouldn't see it.
	writeMD(t, user, "beta", "---\nname: beta\n---\ny")
	got2, _ := l.List()
	if len(got1) != len(got2) {
		t.Errorf("cache should hide new skill; got %d → %d", len(got1), len(got2))
	}
	l.Invalidate()
	got3, _ := l.List()
	if len(got3) <= len(got2) {
		t.Errorf("after Invalidate, beta should appear; got %d", len(got3))
	}
}

func TestLoader_RenderForPrompt(t *testing.T) {
	user := t.TempDir()
	writeMD(t, user, "code-review", `---
name: code-review
description: review code
when_to_use: PR opened
---
body`)
	l := NewLoader(user, "", nil)
	out, err := l.RenderForPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "**code-review**") {
		t.Errorf("missing skill listing: %s", out)
	}
	if !strings.Contains(out, "use when: PR opened") {
		t.Errorf("when_to_use hint missing: %s", out)
	}
}

func TestLoader_DirLayerMissing(t *testing.T) {
	// Non-existent dir should be silently skipped, not error.
	l := NewLoader("/nonexistent/path/nope", "", nil)
	if _, err := l.List(); err != nil {
		t.Errorf("missing user dir should not error; got %v", err)
	}
}
