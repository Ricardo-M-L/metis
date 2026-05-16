package agent

import (
	"fmt"
	"strings"
	"testing"
)

// readFormat mirrors internal/tools/builtin/read.go's "%6d\t%s\n"
// line format so the body gate sees the same shape the real tool
// emits. Keep in sync if Read ever changes its line prefix.
func readFormat(lines []string) string {
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, ln)
	}
	return b.String()
}

func TestSkillReadHint_FiresOnSkillManifest(t *testing.T) {
	body := readFormat([]string{
		"---",
		"name: make-pr",
		"description: Compose a PR — summarize commits, draft test plan",
		"---",
		"You are a PR-author assistant. Help the user open a clean PR.",
	})
	cases := []struct {
		name string
		path string
	}{
		{"home skills dir", "/Users/x/.metis/skills/make-pr.md"},
		{"home skills subdir", "/Users/x/.metis/skills/make-pr/SKILL.md"},
		{"project skills dir", "/repo/.metis/skills/foo.md"},
		{"builtin skills dir", "/repo/internal/agent/skills/builtin/make-pr.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := skillReadHint(c.path, body)
			if got == "" {
				t.Errorf("expected hint for %q, got empty", c.path)
			}
			if !strings.Contains(got, "skill manifest") {
				t.Errorf("hint should mention skill manifest; got:\n%s", got)
			}
		})
	}
}

func TestSkillReadHint_QuietWhenNotSkillPath(t *testing.T) {
	body := readFormat([]string{
		"---",
		"name: not-a-skill",
		"description: but lives outside a skills dir",
		"---",
		"body",
	})
	quiet := []string{
		"/etc/hosts",
		"/repo/README.md",
		"/repo/docs/blog/post.md", // generic frontmatter, not a skill
		"/Users/x/notes.md",
		"", // missing path
	}
	for _, p := range quiet {
		t.Run(p, func(t *testing.T) {
			if got := skillReadHint(p, body); got != "" {
				t.Errorf("expected no hint for %q; got:\n%s", p, got)
			}
		})
	}
}

func TestSkillReadHint_QuietWhenBodyMissingFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"no frontmatter", readFormat([]string{"# heading", "some prose"})},
		{"opens but no name", readFormat([]string{"---", "description: x", "---"})},
		{"closes before name", readFormat([]string{"---", "---", "name: foo"})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := skillReadHint("/Users/x/.metis/skills/foo.md", c.body)
			if got != "" {
				t.Errorf("expected no hint for %s; got:\n%s", c.name, got)
			}
		})
	}
}

func TestSkillReadHint_RecognizesProjectLocalSkillsDir(t *testing.T) {
	body := readFormat([]string{
		"---",
		"name: deploy",
		"description: deploy procedure",
		"---",
	})
	got := skillReadHint("./.metis/skills/deploy.md", body)
	if got == "" {
		t.Error("project-local skills dir should match")
	}
}

func TestPathIsSkillManifest_RequiresSkillsSegment(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/x/skills/foo.md", true},
		{"/x/skills/sub/SKILL.md", true},
		{"/x/.metis/skills/foo.md", true},
		{"/x/foo.md", false},
		{"/x/foo.txt", false},
		{"/x/skills.md", false},        // file named skills.md, not under dir
		{"/x/no-skills/foo.md", false}, // no exact /skills/ segment
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := pathIsSkillManifest(c.path); got != c.want {
				t.Errorf("pathIsSkillManifest(%q) = %v; want %v", c.path, got, c.want)
			}
		})
	}
}

func TestWrapAsSystemReminder_WrapsSkillHint(t *testing.T) {
	hint := skillReadHint(
		"/x/.metis/skills/foo.md",
		readFormat([]string{"---", "name: foo", "description: bar", "---"}),
	)
	wrapped := wrapAsSystemReminder(hint)
	if !strings.Contains(wrapped, "<system-reminder>") || !strings.Contains(wrapped, "</system-reminder>") {
		t.Errorf("wrapped hint missing reminder tags; got:\n%s", wrapped)
	}
	if !strings.Contains(wrapped, "skill manifest") {
		t.Errorf("wrapped hint missing skill copy; got:\n%s", wrapped)
	}
}
