package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsJunkFilename — table-driven coverage of the single source of
// truth filter. All three load paths (embedded.go, loader.go, store.go)
// route through this; if anything regresses, this test catches it
// before the ImageView or `metis skills list` does.
func TestIsJunkFilename(t *testing.T) {
	cases := []struct {
		name string
		junk bool
	}{
		// macOS AppleDouble resource forks
		{"._api-design.md", true},
		{"._foo.json", true},
		{"._.DS_Store", true}, // also matches `._` prefix
		// Editor backups
		{"~api-design.md", true},
		{"~scratch.md", true},
		// macOS Finder metadata
		{".DS_Store", true},
		// Real files — must NOT be filtered
		{"api-design.md", false},
		{"git-bisect.md", false},
		{"installed.json", false},
		{"_underscore.md", false}, // leading single underscore is fine
		{".dotfile.md", false},    // leading dot alone is fine (don't over-filter)
		{"foo._bar.md", false},    // `._` only at prefix counts
		{"weird~name.md", false},  // `~` only at prefix counts
		{"DS_Store", false},       // missing leading dot
		{"my.DS_Store.md", false}, // contains but isn't equal
		{"", false},               // empty handled by callers
	}
	for _, tc := range cases {
		got := isJunkFilename(tc.name)
		if got != tc.junk {
			t.Errorf("isJunkFilename(%q) = %v, want %v", tc.name, got, tc.junk)
		}
	}
}

// TestDirLayer_SkipsJunk — integration test for loader.go's dirLayer.
// Drops both real and junk files in a temp dir, verifies only the
// reals come back. This is the path that handles user / project skill
// directories on disk, where macOS tar extractions actually happen.
func TestDirLayer_SkipsJunk(t *testing.T) {
	dir := t.TempDir()

	// Three legit skills in different valid extensions
	writeMD(t, dir, "real-one", `---
name: real-one
description: a legit skill
---
body
`)
	writeMD(t, dir, "real-two", `---
name: real-two
description: another legit skill
---
body
`)

	// Five junk files that previously polluted the bundle
	junk := []string{
		"._real-one.md",    // resource fork mirroring a real file
		"._ghost.md",       // resource fork with no real counterpart
		"~scratch.md",      // editor backup
		"._installed.json", // resource fork on installed-skill JSON
		".DS_Store",        // Finder metadata
	}
	for _, n := range junk {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	layer := dirLayer("test", 1, dir, "")
	skills, err := layer.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}

	if len(skills) != 2 {
		t.Fatalf("expected exactly 2 skills, got %d: %+v", len(skills), names)
	}
	if !names["real-one"] || !names["real-two"] {
		t.Errorf("missing expected skills; got %+v", names)
	}
	for _, n := range junk {
		// `._real-one.md` would have collided with `real-one` in the
		// names map either way — we assert by Cardinality above. Also
		// double-check no skill name equals a junk basename.
		base := strings.TrimSuffix(n, filepath.Ext(n))
		if names[base] {
			t.Errorf("junk file %q leaked into skill list as %q", n, base)
		}
	}
}

// TestStoreList_SkipsJunk — integration test for Store.List, which
// reads installed-skill JSON. macOS tar of ~/.metis/skills/installed/
// would otherwise leak `._foo.json`.
func TestStoreList_SkipsJunk(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)

	good := &Skill{
		Name:        "real-installed",
		Description: "an installed skill",
		Prompt:      "do the thing",
		Version:     "1.0.0",
	}
	if err := store.Save(good); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Drop ._real-installed.json directly (mimic a tarball extraction
	// that brought a resource fork along with the real JSON).
	if err := os.WriteFile(
		filepath.Join(root, "._real-installed.json"),
		[]byte(`{"name":"junk","description":"should not load"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// Also a non-mirrored junk JSON.
	if err := os.WriteFile(
		filepath.Join(root, "._other.json"),
		[]byte(`{"name":"other-junk"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	got, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 installed skill, got %d: %+v", len(got), got)
	}
	if got[0].Name != "real-installed" {
		t.Errorf("wrong skill loaded: %+v", got[0])
	}
}

// TestBundledLayer_NoJunkInProductionBundle — the production embed.FS
// must not currently contain any junk files. If a contributor ever
// commits a `._foo.md` into builtin/, this test fails at CI time
// rather than waiting for `metis skills list` to garble in the wild.
func TestBundledLayer_NoJunkInProductionBundle(t *testing.T) {
	skills, err := bundledLayer().Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, s := range skills {
		if isJunkFilename(s.Name + ".md") {
			t.Errorf("bundled skill name %q parses as junk — embedded layer leaked it", s.Name)
		}
		if strings.HasPrefix(s.Name, "._") {
			t.Errorf("bundled skill %q has resource-fork prefix", s.Name)
		}
	}
}
