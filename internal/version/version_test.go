package version

// Pin the version-string normalization that the help tab and
// `metis version` (default form) rely on. Originally lived in
// internal/tui/render_welcome_test.go testing the now-deleted
// shortSemver helper there; relocated here when the helper got
// promoted into this package.

import "testing"

func TestShortenSemver(t *testing.T) {
	cases := map[string]string{
		"0.1.3":                    "0.1.3",
		"v0.1.3":                   "0.1.3",
		"0.1.3-21-gab7a825":        "0.1.3",
		"v0.1.3-21-gab7a825-dirty": "0.1.3",
		"":                         "",
	}
	for in, want := range cases {
		if got := shortenSemver(in); got != want {
			t.Errorf("shortenSemver(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShort_ReadsGlobal(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })
	Version = "v9.9.9-42-gdeadbeef-dirty"
	if got := Short(); got != "9.9.9" {
		t.Errorf("Short() = %q, want 9.9.9", got)
	}
}
