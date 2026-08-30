package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLatestSupportsAnonymousPublicRelease(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/repos/"+Repo()+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous request sent Authorization header %q", got)
		}
		_ = json.NewEncoder(w).Encode(release{TagName: "v9.9.9"})
	})
	mux.HandleFunc("/"+Repo()+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+Repo()+"/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("/"+Repo()+"/releases/tag/v9.9.9", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	oldAPI := apiBase
	oldWeb := webBase
	apiBase = server.URL
	webBase = server.URL
	t.Cleanup(func() { apiBase, webBase = oldAPI, oldWeb })

	got, err := Latest(context.Background(), "")
	if err != nil {
		t.Fatalf("Latest without token: %v", err)
	}
	if got.TagName != "v9.9.9" {
		t.Fatalf("Latest tag = %q, want v9.9.9", got.TagName)
	}
}

func TestLatestUsesTokenWhenProvided(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/repos/"+Repo()+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		_ = json.NewEncoder(w).Encode(release{TagName: "v9.9.9"})
	})

	oldAPI := apiBase
	apiBase = server.URL
	t.Cleanup(func() { apiBase = oldAPI })

	if _, err := Latest(context.Background(), "test-token"); err != nil {
		t.Fatalf("Latest with token: %v", err)
	}
}

func TestMaybeCheckSupportsAnonymousPublicRelease(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // keep Token from finding a logged-in gh CLI
	resetGhTokenCache()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/repos/"+Repo()+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{TagName: "v9.9.9"})
	})
	mux.HandleFunc("/"+Repo()+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+Repo()+"/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("/"+Repo()+"/releases/tag/v9.9.9", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	oldAPI := apiBase
	oldWeb := webBase
	apiBase = server.URL
	webBase = server.URL
	t.Cleanup(func() { apiBase, webBase = oldAPI, oldWeb })

	if got := MaybeCheck(context.Background(), t.TempDir(), "0.1.0"); got != "v9.9.9" {
		t.Fatalf("MaybeCheck without token = %q, want v9.9.9", got)
	}
}

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

func TestNormalizeVersionAllowsSemverBuildMetadata(t *testing.T) {
	got, err := normalizeVersion("v1.2.3+build.7")
	if err != nil || got != "1.2.3+build.7" {
		t.Fatalf("normalizeVersion = %q, %v", got, err)
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

// TestTokenGhFallback locks the gh-CLI fallback path without consulting the
// developer machine's credential store. Unit tests must never invoke a real
// `gh auth token`: on macOS that can open a Keychain dialog.
func TestTokenGhFallback(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	oldRunner := ghAuthTokenRunner
	ghAuthTokenRunner = func(context.Context) (string, error) {
		return "gho_test-token-that-never-leaves-the-test", nil
	}
	t.Cleanup(func() { ghAuthTokenRunner = oldRunner })
	resetGhTokenCache()
	if got := Token(); got != "gho_test-token-that-never-leaves-the-test" {
		t.Fatalf("Token() = %q, want mocked gh token", got)
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
// startup (30min throttle), it must refresh the latest_version hint
// from the cached state file so the chrome row keeps showing
// "latest" across throttle windows.
func TestMaybeCheck_WritesLatestVersionFromCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_GITHUB_TOKEN", "fake-token-for-throttle-path")
	// Pre-populate state so MaybeCheck takes the throttle-cache branch.
	saveState(statePath(dir), checkState{
		LastCheck: time.Now().Add(-time.Minute), // within minInterval (30min)
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
