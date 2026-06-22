package builtin

import "testing"

func TestUnderDir(t *testing.T) {
	cases := []struct {
		path, dir string
		want      bool
	}{
		{"/a/skills/foo.md", "/a/skills", true},
		{"/a/skills/sub/foo.md", "/a/skills", true},
		{"/a/skills-shared/foo.md", "/a/skills", false}, // sibling prefix must NOT match
		{"/a/skills", "/a/skills", false},               // the dir itself is not "under"
		{"/b/foo.md", "/a/skills", false},
	}
	for _, c := range cases {
		if got := underDir(c.path, c.dir); got != c.want {
			t.Errorf("underDir(%q,%q)=%v want %v", c.path, c.dir, got, c.want)
		}
	}
}
