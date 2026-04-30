package update

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		have, want string
		newer      bool
	}{
		{"0.1.0", "0.1.1", true},
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "1.0.0", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.1", "0.1.0", false},
		{"v0.1.0", "v0.1.1", true},
		{"0.1.0", "v0.1.1", true},
		{"0.1.0-rc1", "0.1.0", true},
		{"0.1.0", "0.1.0-rc1", false},
		{"0.1.0-rc1", "0.1.0-rc2", true},
		{"0.10.0", "0.9.0", false}, // numeric, not lexicographic
		{"0.9.0", "0.10.0", true},
	}
	for _, tc := range cases {
		got := IsNewer(tc.have, tc.want)
		if got != tc.newer {
			t.Errorf("IsNewer(%q,%q) = %v, want %v", tc.have, tc.want, got, tc.newer)
		}
	}
}

func TestTokenEnvFallback(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "fallback")
	if Token() != "fallback" {
		t.Errorf("expected fallback, got %q", Token())
	}
	t.Setenv("METIS_GITHUB_TOKEN", "primary")
	if Token() != "primary" {
		t.Errorf("expected primary, got %q", Token())
	}
}

func TestRepoOverride(t *testing.T) {
	t.Setenv("METIS_REPO", "")
	if Repo() != "Ricardo-M-L/metis" {
		t.Errorf("default repo wrong: %q", Repo())
	}
	t.Setenv("METIS_REPO", "foo/bar")
	if Repo() != "foo/bar" {
		t.Errorf("override repo wrong: %q", Repo())
	}
}
