package tui

// Banner-pass tests — version, cwd prettifier, and transcript prologue
// emits expected substrings.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShortSemver_StripsGitDescribe used to live here; relocated to
// internal/version/version_test.go when shortSemver got promoted into
// the version package.

// TestPrettifyCwd_ReturnsAbsolutePath — user-facing change 2026-05-09:
// the `~` substitution was making the home cwd visually invisible in
// the welcome card (just a single tilde glyph). prettifyCwd now
// returns the path verbatim so the user can always see exactly which
// directory metis is operating from.
func TestPrettifyCwd_ReturnsAbsolutePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir failed")
	}
	cases := []struct {
		in   string
		want string
	}{
		{home, home},
		{filepath.Join(home, "code", "metis"), filepath.Join(home, "code", "metis")},
		{"/tmp", "/tmp"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := prettifyCwd(tc.in); got != tc.want {
			t.Errorf("prettifyCwd(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderWelcomeBannerCard_ContainsModelAndCwd(t *testing.T) {
	m := &Model{model: "claude-opus-4-7", width: 120}
	got := m.renderWelcomeBannerCard()
	if !strings.Contains(got, "metis") {
		t.Errorf("expected 'metis' in welcome card; got: %q", got)
	}
	if !strings.Contains(got, "claude-opus-4-7") {
		t.Errorf("expected model in welcome card; got: %q", got)
	}
	if !strings.Contains(got, prettifyCwd(currentCwd())) {
		t.Errorf("expected cwd in welcome card; got: %q", got)
	}
}
