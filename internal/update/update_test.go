package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	resetGhTokenCache()
	if Token() != "fallback" {
		t.Errorf("expected fallback, got %q", Token())
	}
	t.Setenv("METIS_GITHUB_TOKEN", "primary")
	resetGhTokenCache()
	if Token() != "primary" {
		t.Errorf("expected primary, got %q", Token())
	}
}

// TestTokenGhFallback locks the gh-CLI fallback path: when neither
// env var is set, Token() shells out to `gh auth token`. We can't
// assume gh is logged in on every CI runner, so the assertion is
// conditional — when gh is absent / unauthenticated the result is
// "" (which is the legitimate "no token available" state).
func TestTokenGhFallback(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	resetGhTokenCache()
	got := Token()
	// On a CI machine without gh, this is "". On a dev machine with
	// `gh auth login` already done, it's a real token. Either is fine
	// — what matters is the function doesn't panic and the lookup
	// terminates within the 2s ghAuthToken timeout.
	if got != "" && len(got) < 20 {
		t.Errorf("gh token suspiciously short: %q (len=%d)", got, len(got))
	}
}

// resetGhTokenCache clears the per-process memoization between
// subtests. Without this, ghAuthToken would remember the value from
// the first call and skip the env-var override the second test
// sets up. Test-only — not exported.
func resetGhTokenCache() {
	ghAuthTokenCached = ""
	ghAuthTokenLookedUp = false
	ghAuthTokenLookupErr = nil
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

// TestWriteLatestVersionFile_HappyPath asserts the helper actually
// writes the file the TUI's renderVersionLine reads. Image #2/#3
// regression: the chrome row "current: vX · latest: vY" only lights
// up if this file exists, so a silently-skipped write equals a silent
// missing-feature complaint.
func TestWriteLatestVersionFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeLatestVersionFile(dir, "v0.1.5")
	got, err := os.ReadFile(filepath.Join(dir, latestVersionFile))
	if err != nil {
		t.Fatalf("expected file written; ReadFile: %v", err)
	}
	if string(got) != "v0.1.5\n" {
		t.Errorf("file contents = %q, want %q", got, "v0.1.5\n")
	}
}

func TestWriteLatestVersionFile_SilentOnEmptyArgs(t *testing.T) {
	dir := t.TempDir()
	// Empty home and empty tag both must no-op without panic. The TUI
	// reader treats a missing file as "no latest known", so a silent
	// skip is the correct contract for these inputs.
	writeLatestVersionFile("", "v0.1.5")
	writeLatestVersionFile(dir, "")
	if _, err := os.Stat(filepath.Join(dir, latestVersionFile)); !os.IsNotExist(err) {
		t.Errorf("file should NOT exist after empty-tag call; got err=%v", err)
	}
}

// TestMaybeCheck_WritesLatestVersionFromCache locks in the throttle-
// window code path: even when MaybeCheck doesn't hit the network this
// startup (24h throttle), it must refresh the latest_version hint
// from the cached state file so the chrome row keeps showing
// "latest" across throttle windows.
func TestMaybeCheck_WritesLatestVersionFromCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_GITHUB_TOKEN", "fake-token-for-throttle-path")
	// Pre-populate state so MaybeCheck takes the throttle-cache branch.
	saveState(statePath(dir), checkState{
		LastCheck: time.Now().Add(-time.Hour), // within minInterval (24h)
		LatestTag: "v0.2.0",
		// Mark this tag as already-notified so we don't try to surface it.
		LastTagSeen: "v0.2.0",
	})
	_ = MaybeCheck(nil, dir, "0.2.0") // ctx unused on cache path

	got, err := os.ReadFile(filepath.Join(dir, latestVersionFile))
	if err != nil {
		t.Fatalf("expected latest_version written from cache; ReadFile: %v", err)
	}
	if string(got) != "v0.2.0\n" {
		t.Errorf("cached-write contents = %q, want %q", got, "v0.2.0\n")
	}
}
