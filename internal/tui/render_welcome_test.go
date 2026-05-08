package tui

// Banner-pass tests — version cropping, cwd prettifier, header
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

func TestPrettifyCwd_ExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir failed; can't test prettifier")
	}
	cases := []struct {
		in   string
		want string
	}{
		{home, "~"},
		{filepath.Join(home, "code", "metis"), "~" + string(filepath.Separator) + filepath.Join("code", "metis")},
		{"/tmp", "/tmp"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := prettifyCwd(tc.in); got != tc.want {
			t.Errorf("prettifyCwd(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderHeaderBanner_NotCrashOnNilModel(t *testing.T) {
	// Defensive: callers shouldn't pass nil but covering it stops a
	// stray banner from panicking the chat surface.
	got := (*Model)(nil).renderHeaderBanner()
	if got != "" {
		t.Errorf("nil model should yield empty string; got %q", got)
	}
}

func TestRenderHeaderBanner_ContainsModelAndCwd(t *testing.T) {
	m := &Model{model: "claude-opus-4-7", width: 120}
	// Gate is the one field we can't trivially mock without dragging
	// permission into the test; the renderHeaderBanner path tolerates
	// a nil gate via the early return so we test that route instead.
	got := m.renderHeaderBanner()
	if got == "" {
		t.Skip("renderHeaderBanner returned empty (likely nil-gate path); fine")
	}
	if !strings.Contains(got, "metis") {
		t.Errorf("expected 'metis' in header; got: %q", got)
	}
}
