package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func completeCLIRelease(tag, base string) map[string]any {
	assets := []map[string]any{}
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			ext := ".tar.gz"
			if platform == "windows" {
				ext = ".zip"
			}
			for _, suffix := range []string{"", ".sha256"} {
				name := "metis-" + platform + "-" + arch + ext + suffix
				assets = append(assets, map[string]any{"id": len(assets) + 1, "name": name, "size": 1, "state": "uploaded", "browser_download_url": base + "/" + Repo() + "/releases/download/" + tag + "/" + name})
			}
		}
	}
	return map[string]any{"tag_name": tag, "draft": false, "prerelease": false, "assets": assets}
}

func useReleaseServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldAPI, oldWeb := apiBase, webBase
	apiBase, webBase = srv.URL, srv.URL
	t.Cleanup(func() { apiBase, webBase = oldAPI, oldWeb; srv.Close() })
	return srv
}

func TestStableChannelSelectsHighestAcrossPages(t *testing.T) {
	for _, token := range []string{"", "fake-token"} {
		t.Run("token="+token, func(t *testing.T) {
			calls := 0
			useReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/repos/"+Repo()+"/releases" || r.URL.Query().Get("per_page") != "100" {
					http.Error(w, "shared latest forbidden", 400)
					return
				}
				wantAuth := ""
				if token != "" {
					wantAuth = "Bearer " + token
				}
				if r.Header.Get("Authorization") != wantAuth {
					t.Errorf("wrong Authorization")
				}
				page := []map[string]any{}
				switch r.URL.Query().Get("page") {
				case "1":
					for i := 0; i < 99; i++ {
						page = append(page, map[string]any{"tag_name": "desktop-only"})
					}
					page = append(page, completeCLIRelease("v0.4.9", webBase))
				case "2":
					page = append(page, completeCLIRelease("v0.4.51", webBase), completeCLIRelease("v0.4.47", webBase))
				default:
					t.Errorf("unexpected page %s", r.URL.RawQuery)
				}
				_ = json.NewEncoder(w).Encode(page)
			})
			got, err := Latest(context.Background(), token)
			if err != nil || got.TagName != "v0.4.51" || calls != 2 {
				t.Fatalf("Latest = %v, %v; calls=%d", got, err, calls)
			}
		})
	}
}

func TestStableChannelRejectsIneligibleCandidates(t *testing.T) {
	cases := map[string]func(map[string]any){
		"draft":              func(r map[string]any) { r["draft"] = true },
		"preview":            func(r map[string]any) { r["prerelease"] = true },
		"missing draft":      func(r map[string]any) { delete(r, "draft") },
		"null draft":         func(r map[string]any) { r["draft"] = nil },
		"missing prerelease": func(r map[string]any) { delete(r, "prerelease") },
		"missing checksum":   func(r map[string]any) { r["assets"] = r["assets"].([]map[string]any)[:11] },
		"duplicate":          func(r map[string]any) { a := r["assets"].([]map[string]any); r["assets"] = append(a, a[0]) },
		"empty asset":        func(r map[string]any) { r["assets"].([]map[string]any)[0]["size"] = 0 },
		"unfinished asset":   func(r map[string]any) { r["assets"].([]map[string]any)[0]["state"] = "new" },
		"foreign URL": func(r map[string]any) {
			r["assets"].([]map[string]any)[0]["browser_download_url"] = "https://evil.invalid/download"
		},
		"URL query": func(r map[string]any) {
			a := r["assets"].([]map[string]any)[0]
			a["browser_download_url"] = a["browser_download_url"].(string) + "?raw=1"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			useReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
				bad := completeCLIRelease("v9.9.9", webBase)
				change(bad)
				_ = json.NewEncoder(w).Encode([]any{bad, completeCLIRelease("v0.4.51", webBase)})
			})
			got, err := Latest(context.Background(), "token")
			if err != nil || got.TagName != "v0.4.51" {
				t.Fatalf("Latest=%v, %v", got, err)
			}
		})
	}
}

func TestStableChannelStrictNumericTags(t *testing.T) {
	for _, tag := range []string{"0.4.52", "v00.4.52", "v0.04.52", "v0.4.052", "v0.4.52-rc1", "v0.4.52+build", "v1000000000.0.0", "v0.1000000000.0", "v0.0.1000000000", "v0.4.52\n"} {
		t.Run(tag, func(t *testing.T) {
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode([]any{completeCLIRelease(tag, webBase)})
			})
			if got, err := Latest(context.Background(), "token"); err == nil {
				t.Fatalf("invalid stable tag accepted: %v", got)
			}
		})
	}
}

func TestStableChannelNeverReturnsPartialOrWebFallback(t *testing.T) {
	for _, mode := range []string{"rate-limit", "malformed", "trailing-json", "non-array", "oversize", "page-limit", "too-many-items"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			useReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.Contains(r.URL.Path, "latest") {
					_ = json.NewEncoder(w).Encode(completeCLIRelease("v0.4.47", webBase))
					return
				}
				if r.URL.Query().Get("page") == "1" || mode == "page-limit" {
					page := make([]any, 100)
					for i := range page {
						page[i] = completeCLIRelease("v0.4.51", webBase)
					}
					if mode == "too-many-items" {
						page = append(page, completeCLIRelease("v0.4.52", webBase))
					}
					_ = json.NewEncoder(w).Encode(page)
					return
				}
				switch mode {
				case "rate-limit":
					http.Error(w, "rate limit", 403)
				case "malformed":
					fmt.Fprint(w, "[{broken")
				case "trailing-json":
					fmt.Fprint(w, "[]{}")
				case "non-array":
					fmt.Fprint(w, "null")
				case "oversize":
					fmt.Fprint(w, strings.Repeat(" ", (8<<20)+1)+"[]")
				}
			})
			got, err := Latest(context.Background(), "")
			if err == nil || got != nil {
				t.Fatalf("partial/fallback accepted: %v, %v", got, err)
			}
			if calls > 10 {
				t.Fatalf("unbounded pagination: %d", calls)
			}
		})
	}
}

func TestMaybeCheckMigratesLegacySourceImmediately(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "token")
	dir := t.TempDir()
	saveState(statePath(dir), checkState{LastCheck: time.Now(), LatestTag: "v0.4.47", LastTagSeen: "v0.4.47"})
	calls := 0
	useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode([]any{completeCLIRelease("v0.4.51", webBase)})
	})
	if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "v0.4.51" || calls != 1 {
		t.Fatalf("migration got=%s calls=%d", got, calls)
	}
	var state map[string]any
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	if state["source"] != "cli-stable-v1" {
		t.Fatalf("source=%v", state["source"])
	}
	b, err = os.ReadFile(filepath.Join(dir, latestVersionFile))
	if err != nil || string(b) != "v0.4.51\n" {
		t.Fatalf("latest mirror=%q, %v", b, err)
	}
}

func TestApplyRefusesDowngradeInsideLock(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			oldTarget := targetForTest
			targetForTest = "test-os-arch"
			t.Cleanup(func() { targetForTest = oldTarget })
			srv, rel := buildFakeRelease(t, "0.4.47", targetForTest, fakeExecutable("0.4.47", "old"))
			oldAPI := apiBase
			apiBase = srv.URL
			t.Cleanup(func() { apiBase = oldAPI })
			launcher := filepath.Join(t.TempDir(), "bin", executableName())
			layout := layoutForLauncher(launcher)
			current := versionBinary(layout, "0.4.51+local.commit")
			if err := os.MkdirAll(filepath.Dir(current), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(current, fakeExecutable("0.4.51", "current"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := activateVersion(layout, "0.4.51+local.commit", current); err != nil {
				t.Fatal(err)
			}
			var err error
			if automatic {
				_, err = ApplyIfNeeded(context.Background(), "", launcher, rel)
			} else {
				err = Apply(context.Background(), "", launcher, rel)
			}
			if err == nil || !strings.Contains(err.Error(), "downgrade") {
				t.Fatalf("downgrade error=%v", err)
			}
			if got, ok := resolveCurrentVersion(layout); !ok || got != "0.4.51+local.commit" {
				t.Fatalf("active version changed: %s", got)
			}
		})
	}
}

func TestIsNewerIgnoresBuildMetadata(t *testing.T) {
	if IsNewer("0.4.51+local.commit", "0.4.51") || IsNewer("0.4.51", "0.4.51+build") || !IsNewer("0.4.51+local.commit", "0.4.52") {
		t.Fatal("build metadata changed version ordering")
	}
}

func TestStableChannelSharedContractFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install", "testdata", "cli-releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Repo    string `json:"repo"`
		WebBase string `json:"web_base"`
		Cases   []struct {
			Name     string          `json:"name"`
			Releases json.RawMessage `json:"releases"`
			WantTag  string          `json:"want_tag"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_REPO", fixture.Repo)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, strings.ReplaceAll(string(tc.Releases), fixture.WebBase, webBase))
			})
			got, err := Latest(context.Background(), "token")
			if tc.WantTag == "" {
				if err == nil {
					t.Fatalf("expected no candidate, got %v", got)
				}
				return
			}
			if err != nil || got.TagName != tc.WantTag {
				t.Fatalf("Latest=%v, %v; want %s", got, err, tc.WantTag)
			}
		})
	}
}

func TestStableChannelTotalContextDeadlineAcrossPages(t *testing.T) {
	var calls atomic.Int32
	useReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("page") == "1" {
			page := make([]any, 100)
			for i := range page {
				page[i] = map[string]any{"tag_name": "desktop"}
			}
			page[99] = completeCLIRelease("v0.4.51", webBase)
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := Latest(ctx, "token")
	if err == nil || got != nil || time.Since(start) > time.Second || calls.Load() != 2 {
		t.Fatalf("deadline not shared: got=%v err=%v calls=%d elapsed=%v", got, err, calls.Load(), time.Since(start))
	}
}

func TestMaybeCheckFailedMigrationPreservesKnownCache(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "token")
	dir := t.TempDir()
	saveState(statePath(dir), checkState{LastCheck: time.Now(), LatestTag: "v0.4.47"})
	writeLatestVersionFile(dir, "v0.4.47")
	before, _ := os.ReadFile(statePath(dir))
	useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "rate limit", 403) })
	if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "" {
		t.Fatalf("failure falsely announced %q", got)
	}
	after, _ := os.ReadFile(statePath(dir))
	if string(before) != string(after) {
		t.Fatal("failure rewrote successful-check cache")
	}
	b, _ := os.ReadFile(filepath.Join(dir, latestVersionFile))
	if string(b) != "v0.4.47\n" {
		t.Fatalf("failure fabricated latest_version %q", b)
	}
}

func TestApplyRefusesLegacyLauncherDowngradeBeforeDownload(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix fixture")
	}
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			launcher := filepath.Join(t.TempDir(), "bin", "metis")
			if err := os.MkdirAll(filepath.Dir(launcher), 0755); err != nil {
				t.Fatal(err)
			}
			original := fakeExecutable("0.4.51", "current")
			if err := os.WriteFile(launcher, original, 0755); err != nil {
				t.Fatal(err)
			}
			requests := 0
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) { requests++; http.Error(w, "must not download", 500) })
			r := &release{TagName: "v0.4.47", Assets: []asset{{ID: 1, Name: "metis-" + Target() + ".tar.gz"}, {ID: 2, Name: "metis-" + Target() + ".tar.gz.sha256"}}}
			var err error
			if automatic {
				_, err = ApplyIfNeeded(context.Background(), "token", launcher, r)
			} else {
				err = Apply(context.Background(), "token", launcher, r)
			}
			if err == nil || !strings.Contains(err.Error(), "downgrade") || requests != 0 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
			got, _ := os.ReadFile(launcher)
			if string(got) != string(original) {
				t.Fatal("legacy launcher changed")
			}
		})
	}
}

func TestApplyChecksNewlyActivatedVersionAfterWaitingForLock(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "bin", executableName())
	layout := layoutForLauncher(launcher)
	old := seedManagedVersion(t, layout.versionsRoot, "0.4.47", time.Now())
	newer := seedManagedVersion(t, layout.versionsRoot, "0.4.52", time.Now())
	if err := activateVersion(layout, "0.4.47", old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	unlock, err := acquireInstallLock(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := make(chan error, 1)
	go func() { done <- Apply(ctx, "token", launcher, &release{TagName: "v0.4.51"}) }()
	select {
	case err := <-done:
		t.Fatalf("Apply bypassed owned install lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := activateVersion(layout, "0.4.52", newer); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("stale candidate was not rejected: %v", err)
	}
	if current, ok := resolveCurrentVersion(layout); !ok || current != "0.4.52" {
		t.Fatalf("active version=%s,%v", current, ok)
	}
}

func TestStableChannelHTTPErrorDoesNotEchoResponseBody(t *testing.T) {
	useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "untrusted-body-with-secret", 403) })
	_, err := Latest(context.Background(), "token")
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "untrusted-body-with-secret") {
		t.Fatalf("unsafe HTTP error: %v", err)
	}
}

func TestStableChannelNumericAssetSizeContract(t *testing.T) {
	for _, tc := range []struct {
		size  string
		valid bool
	}{{"1.0", true}, {"1e2", true}, {"0.5", false}, {"true", false}, {`"1"`, false}, {"null", false}, {"1e999", false}, {"9223372036854775808", false}} {
		t.Run(tc.size, func(t *testing.T) {
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
				candidate := completeCLIRelease("v0.4.52", webBase)
				candidate["assets"].([]map[string]any)[0]["size"] = json.RawMessage(tc.size)
				_ = json.NewEncoder(w).Encode([]any{candidate, completeCLIRelease("v0.4.51", webBase)})
			})
			want := "v0.4.51"
			if tc.valid {
				want = "v0.4.52"
			}
			got, err := Latest(context.Background(), "token")
			if err != nil || got.TagName != want {
				t.Fatalf("Latest=%v,%v want=%s", got, err, want)
			}
		})
	}
}

func TestStableChannelWrongTypeCandidateDoesNotAbortPage(t *testing.T) {
	for _, field := range []string{"tag_name", "draft", "prerelease", "assets"} {
		t.Run(field, func(t *testing.T) {
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
				bad := completeCLIRelease("v0.4.52", webBase)
				bad[field] = 123
				_ = json.NewEncoder(w).Encode([]any{bad, completeCLIRelease("v0.4.51", webBase)})
			})
			got, err := Latest(context.Background(), "token")
			if err != nil || got.TagName != "v0.4.51" {
				t.Fatalf("Latest=%v,%v", got, err)
			}
		})
	}
}

func TestStableChannelInvalidEntriesKeepPaginationLength(t *testing.T) {
	useReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			page := make([]any, 100)
			for i := range page {
				page[i] = 123
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		_ = json.NewEncoder(w).Encode([]any{completeCLIRelease("v0.4.51", webBase)})
	})
	got, err := Latest(context.Background(), "token")
	if err != nil || got.TagName != "v0.4.51" {
		t.Fatalf("Latest=%v,%v", got, err)
	}
}

func TestStableChannelExtraDesktopFieldsAreNotCLIValidation(t *testing.T) {
	useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		r := completeCLIRelease("v0.4.51", webBase)
		r["assets"] = append(r["assets"].([]map[string]any), map[string]any{"name": "Metis.Desktop.dmg", "size": true, "state": 42, "browser_download_url": false})
		_ = json.NewEncoder(w).Encode([]any{r})
	})
	got, err := Latest(context.Background(), "token")
	if err != nil || got.TagName != "v0.4.51" {
		t.Fatalf("Latest=%v,%v", got, err)
	}
}

func TestMaybeCheckSourceIdentityChangeBypassesThrottle(t *testing.T) {
	for _, change := range []string{"repo", "api", "web"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("METIS_GITHUB_TOKEN", "token")
			t.Setenv("METIS_REPO", "old/repo")
			dir := t.TempDir()
			tag := "v0.4.51"
			calls := 0
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode([]any{completeCLIRelease(tag, webBase)})
			})
			if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "v0.4.51" {
				t.Fatalf("initial=%s", got)
			}
			tag = "v0.4.52"
			switch change {
			case "repo":
				t.Setenv("METIS_REPO", "new/repo")
			case "api":
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					_ = json.NewEncoder(w).Encode([]any{completeCLIRelease(tag, webBase)})
				}))
				t.Cleanup(srv.Close)
				apiBase = srv.URL
			case "web":
				webBase = "https://mirror.invalid"
			}
			if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "v0.4.52" || calls != 2 {
				t.Fatalf("changed source reused cache: got=%s calls=%d", got, calls)
			}
			var state map[string]any
			b, _ := os.ReadFile(statePath(dir))
			_ = json.Unmarshal(b, &state)
			key, _ := state["source_key"].(string)
			if len(key) != 64 {
				t.Fatalf("source fingerprint=%q", key)
			}
		})
	}
}

func TestMaybeCheckNewSourceResetsNotificationHistory(t *testing.T) {
	t.Setenv("METIS_GITHUB_TOKEN", "token")
	t.Setenv("METIS_REPO", "old/repo")
	dir := t.TempDir()
	useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{completeCLIRelease("v0.4.51", webBase)})
	})
	if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "v0.4.51" {
		t.Fatalf("initial=%s", got)
	}
	MarkNotified(dir, "v0.4.51")
	t.Setenv("METIS_REPO", "new/repo")
	if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "v0.4.51" {
		t.Fatalf("new source=%s", got)
	}
	state := loadState(statePath(dir))
	if state.LastTagSeen != "" || !state.LastNotify.IsZero() {
		t.Fatalf("old source notification retained: %+v", state)
	}
	if got := MaybeCheck(context.Background(), dir, "0.4.47"); got != "v0.4.51" {
		t.Fatalf("old source suppressed cached notification: %s", got)
	}
}

func TestCLIStableSourceKeyNormalizesTrailingSlashes(t *testing.T) {
	oldAPI, oldWeb := apiBase, webBase
	t.Cleanup(func() { apiBase, webBase = oldAPI, oldWeb })
	apiBase, webBase = "https://api.example.invalid", "https://downloads.example.invalid"
	want := cliStableSourceKey()
	apiBase += "/"
	webBase += "/"
	if got := cliStableSourceKey(); got != want {
		t.Fatal("equivalent trailing slashes changed identity")
	}
}
