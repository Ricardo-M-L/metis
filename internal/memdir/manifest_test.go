package memdir

import (
	"strings"
	"testing"
	"time"
)

func TestFormatManifest_Empty(t *testing.T) {
	got := FormatManifest("/r", nil)
	if !strings.Contains(got, "(empty)") {
		t.Fatalf("expected (empty) marker, got %q", got)
	}
}

func TestFormatManifest_GroupsByType(t *testing.T) {
	files := []MemoryFile{
		{Path: "/r/user_role.md", Name: "user_role", ModTime: time.Now(),
			Frontmatter: Frontmatter{Name: "User Role", Description: "be lead", Type: TypeUser}},
		{Path: "/r/feedback_x.md", Name: "feedback_x", ModTime: time.Now(),
			Frontmatter: Frontmatter{Name: "Fb", Description: "do x", Type: TypeFeedback}},
		{Path: "/r/unc.md", Name: "unc", ModTime: time.Now(),
			Frontmatter: Frontmatter{Name: "U", Description: "no type"}},
		{Path: "/r/bad.md", Name: "bad", ParseError: errFakeParse},
	}
	got := FormatManifest("/r", files)
	for _, want := range []string{
		"## user (1)", "## feedback (1)", "## unclassified (1)",
		"## errors (1)", "user_role.md — be lead", "bad.md — parse: parse",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}
}

func TestFormatManifest_HeaderCount(t *testing.T) {
	files := []MemoryFile{
		{Path: "/r/u1.md", ModTime: time.Now(), Frontmatter: Frontmatter{Description: "x", Type: TypeUser}},
		{Path: "/r/u2.md", ModTime: time.Now(), Frontmatter: Frontmatter{Description: "x", Type: TypeUser}},
	}
	got := FormatManifest("/r", files)
	if !strings.Contains(got, "/r: 2 memories") {
		t.Errorf("header missing count: %q", got)
	}
	if !strings.Contains(got, "(2 user)") {
		t.Errorf("breakdown missing: %q", got)
	}
}
