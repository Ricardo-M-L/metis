package runtime

// builtin_profiles_test.go — locks the G.7 (2026-05-12) bundled agent
// profile contract:
//
//   1. All 6 expected profiles are bundled and parseable.
//   2. The frontmatter for each maps to the right toolset (so a
//      future "I'll just edit the markdown" change can't quietly
//      drop a critical tool from `explore`).
//   3. Unknown profile names return ErrBuiltinProfileNotFound, NOT
//      a parse error — caller behavior depends on that distinction.
//   4. LoadAgentProfile falls back to the bundled set after user/
//      project dirs miss.
//   5. User overrides win — a file under .metis/agents/ wins even
//      when bundled has the same name.

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestBuiltinProfileNames_All9Present(t *testing.T) {
	t.Parallel()
	got := BuiltinProfileNames()
	// 9 total: 6 from G.7 + coordinator from G.8 (2026-05-12) +
	// teammate from 2026-05-16 (Team-paradigm-aware profile that
	// bundles MessageTeammate + Task* + base tools so a coordinated
	// team member doesn't have to guess "do I have peer messaging?") +
	// the Desktop-oriented creator preset.
	want := []string{
		"coordinator",
		"creator",
		"explore",
		"general",
		"go-reviewer",
		"mcp-debugger",
		"plan",
		"teammate",
		"verify",
	}
	if len(got) != len(want) {
		t.Fatalf("BuiltinProfileNames: got %v, want %v", got, want)
	}
	sort.Strings(got)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BuiltinProfileNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuiltinProfile_FrontmatterIsParsed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		wantTools         []string
		wantPermissionMod string
	}{
		{"explore", []string{"Read", "Grep", "Glob", "LS", "WebFetch"}, "bypassPermissions"},
		{"plan", []string{"Read", "Grep", "Glob", "LS", "WebFetch"}, "bypassPermissions"},
		{"verify", []string{"Read", "Bash", "Grep", "Glob", "LS"}, "bypassPermissions"},
		{"go-reviewer", []string{"Read", "Grep", "Glob", "LS", "Bash"}, "bypassPermissions"},
		{"mcp-debugger", []string{"Read", "Grep", "Glob", "LS", "Bash", "MetisInfo"}, "bypassPermissions"},
		{"creator", nil, "acceptEdits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prof, err := LoadBuiltinProfile(c.name)
			if err != nil {
				t.Fatalf("LoadBuiltinProfile(%q): %v", c.name, err)
			}
			if prof.Name != c.name {
				t.Errorf("Name = %q, want %q", prof.Name, c.name)
			}
			if prof.PermissionMode != c.wantPermissionMod {
				t.Errorf("PermissionMode = %q, want %q", prof.PermissionMode, c.wantPermissionMod)
			}
			if got := sliceEqual(prof.Tools, c.wantTools); !got {
				t.Errorf("Tools = %v, want %v", prof.Tools, c.wantTools)
			}
			if prof.SystemPrompt == "" {
				t.Error("SystemPrompt should be non-empty")
			}
		})
	}
}

// TestBuiltinProfile_GeneralHasNoToolFilter — the `general` profile
// explicitly inherits the parent's toolset by omitting `tools:`.
// Locks the "no filter when empty" semantic.
func TestBuiltinProfile_GeneralHasNoToolFilter(t *testing.T) {
	t.Parallel()
	prof, err := LoadBuiltinProfile("general")
	if err != nil {
		t.Fatalf("LoadBuiltinProfile: %v", err)
	}
	if len(prof.Tools) != 0 {
		t.Errorf("general profile should have empty Tools (inherit); got %v", prof.Tools)
	}
}

func TestLoadBuiltinProfile_UnknownReturnsSentinel(t *testing.T) {
	t.Parallel()
	_, err := LoadBuiltinProfile("definitely-not-a-real-profile-name")
	if !errors.Is(err, ErrBuiltinProfileNotFound) {
		t.Errorf("expected ErrBuiltinProfileNotFound; got %v", err)
	}
}

// TestLoadAgentProfile_FallsBackToBundled — no user/project file
// exists but the name is in the bundled set, so the cascade should
// pick up the bundled profile.
func TestLoadAgentProfile_FallsBackToBundled(t *testing.T) {
	// CD into a tmpdir so the project ".metis/agents/" lookup misses.
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	prof, err := LoadAgentProfile("explore")
	if err != nil {
		t.Fatalf("LoadAgentProfile(explore): %v", err)
	}
	if prof == nil {
		t.Fatal("expected non-nil profile from bundled fallback")
	}
	if prof.Name != "explore" {
		t.Errorf("Name = %q, want explore", prof.Name)
	}
	if prof.SystemPrompt == "" {
		t.Error("bundled fallback SystemPrompt is empty")
	}
}

// TestLoadAgentProfile_UserFileWinsOverBundled — drop a custom
// explore.md in project's .metis/agents/, confirm it wins over the
// bundled one.
func TestLoadAgentProfile_UserFileWinsOverBundled(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".metis", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := "---\nname: explore\ndescription: CUSTOM\n---\nCustom body for explore.\n"
	if err := os.WriteFile(filepath.Join(dir, "explore.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	prof, err := LoadAgentProfile("explore")
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if prof.Description != "CUSTOM" {
		t.Errorf("user override should have Description=CUSTOM; got %q", prof.Description)
	}
	if prof.SystemPrompt != "Custom body for explore." {
		t.Errorf("user override SystemPrompt = %q", prof.SystemPrompt)
	}
}

// sliceEqual reports whether a and b contain the same strings in
// the same order. Used in the tools-equality assertion above.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
