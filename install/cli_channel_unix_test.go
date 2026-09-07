package install_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func unixCLIRelease(tag, webBase string) map[string]any {
	assets := []map[string]any{}
	for _, target := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64", "windows-amd64", "windows-arm64"} {
		ext := ".tar.gz"
		if strings.HasPrefix(target, "windows-") {
			ext = ".zip"
		}
		name := "metis-" + target + ext
		for _, asset := range []string{name, name + ".sha256"} {
			assets = append(assets, map[string]any{"name": asset, "size": 12, "state": "uploaded", "browser_download_url": webBase + "/Ricardo-M-L/metis/releases/download/" + tag + "/" + asset})
		}
	}
	return map[string]any{"tag_name": tag, "draft": false, "prerelease": false, "assets": assets}
}

// The stripped script runs the actual installer functions, without any install
// or launcher mutation. A private PATH exercises each supported JSON parser.
func unixCLIResolver(t *testing.T, parser, apiBase, webBase, token string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Bash installer is for macOS and Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	privatePath := t.TempDir()
	for _, name := range []string{"curl", "date", "head", "stat", "rm", parser} {
		if name == "none" {
			continue
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is unavailable: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(privatePath, name)); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.TrimSuffix(string(script), "main \"$@\"\n")
	if source == string(script) {
		t.Fatal("installer main invocation not found")
	}
	cmd := exec.Command(bash, "-c", source+"\nTMPDIR_PATH=\"$TEST_METADATA_DIR\"\nresolve_release_tag \"$METIS_VERSION\"\n")
	cmd.Env = append(withoutEnv(os.Environ(), "PATH", "METIS_GITHUB_TOKEN", "GITHUB_TOKEN", "METIS_VERSION", "METIS_REPO", "METIS_GITHUB_API_BASE", "METIS_GITHUB_WEB_BASE"),
		"PATH="+privatePath,
		"TEST_METADATA_DIR="+t.TempDir(),
		"METIS_VERSION=latest",
		"METIS_REPO=Ricardo-M-L/metis",
		"METIS_GITHUB_API_BASE="+apiBase,
		"METIS_GITHUB_WEB_BASE="+webBase,
		"METIS_GITHUB_TOKEN="+token,
	)
	return cmd
}

func TestUnixCLIChannelIgnoresSharedLatest(t *testing.T) {
	for _, parser := range []string{"jq", "python3"} {
		for _, token := range []string{"", "test-token-not-secret"} {
			t.Run(parser+fmt.Sprint(token != ""), func(t *testing.T) {
				var forbidden atomic.Int32
				var server *httptest.Server
				server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/repos/Ricardo-M-L/metis/releases" || r.URL.RawQuery != "per_page=100&page=1" {
						forbidden.Add(1)
						if strings.HasSuffix(r.URL.Path, "/latest") {
							if strings.HasPrefix(r.URL.Path, "/repos/") {
								fmt.Fprint(w, `{"tag_name":"v0.4.47"}`)
							} else {
								http.Redirect(w, r, "/Ricardo-M-L/metis/releases/tag/v0.4.47", http.StatusFound)
							}
						}
						return
					}
					wantAuth := ""
					if token != "" {
						wantAuth = "Bearer " + token
					}
					if r.Header.Get("Authorization") != wantAuth {
						t.Error("unexpected authorization header")
					}
					_ = json.NewEncoder(w).Encode([]any{unixCLIRelease("v0.4.47", server.URL), unixCLIRelease("v0.4.51", server.URL)})
				}))
				server.Start()
				defer server.Close()
				out, err := unixCLIResolver(t, parser, server.URL, server.URL, token).CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "v0.4.51" || forbidden.Load() != 0 {
					t.Fatalf("resolution = %q, err=%v, forbidden requests=%d", out, err, forbidden.Load())
				}
			})
		}
	}
}

func TestUnixCLIChannelRateLimitDoesNotInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash installer is for macOS and Linux")
	}
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var unexpected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/Ricardo-M-L/metis/releases" {
					unexpected.Add(1)
				}
				http.Error(w, "rate limited", status)
			}))
			defer server.Close()
			base := t.TempDir()
			cmd := exec.Command("bash", "install.sh")
			cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"), "METIS_GITHUB_API_BASE="+server.URL,
				"METIS_GITHUB_WEB_BASE="+server.URL, "METIS_INSTALL_DIR="+filepath.Join(base, "bin"), "METIS_VERSION=latest", "METIS_REPO=Ricardo-M-L/metis")
			out, err := cmd.CombinedOutput()
			if err == nil || unexpected.Load() != 0 || !strings.Contains(string(out), "METIS_GITHUB_TOKEN") || !strings.Contains(string(out), "METIS_VERSION") {
				t.Fatalf("unexpected failure: %v, requests=%d, output=%s", err, unexpected.Load(), out)
			}
			if _, err := os.Lstat(filepath.Join(base, "bin", "metis")); !os.IsNotExist(err) {
				t.Fatalf("failed discovery activated a launcher: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(base, "share", "metis", "versions"))
			if err != nil && !os.IsNotExist(err) || len(entries) != 0 {
				t.Fatalf("failed discovery installed versions: %v, %v", entries, err)
			}
		})
	}
}

func TestUnixCLIChannelSharedFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/cli-releases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		WebBase string `json:"web_base"`
		Cases   []struct {
			Name     string          `json:"name"`
			Releases json.RawMessage `json:"releases"`
			WantTag  string          `json:"want_tag"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, parser := range []string{"jq", "python3"} {
		for _, tc := range fixture.Cases {
			t.Run(parser+"/"+tc.Name, func(t *testing.T) {
				var requests atomic.Int32
				var server *httptest.Server
				server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.URL.Path != "/repos/Ricardo-M-L/metis/releases" || r.URL.RawQuery != "per_page=100&page=1" {
						t.Errorf("unexpected request %s", r.URL)
						http.NotFound(w, r)
						return
					}
					fmt.Fprint(w, strings.ReplaceAll(string(tc.Releases), fixture.WebBase, server.URL))
				}))
				server.Start()
				defer server.Close()
				out, err := unixCLIResolver(t, parser, server.URL, server.URL, "").CombinedOutput()
				if tc.WantTag == "" {
					if err == nil || !strings.Contains(string(out), "no complete stable CLI release") {
						t.Fatalf("expected no eligible release: err=%v, output=%s", err, out)
					}
				} else if err != nil || strings.TrimSpace(string(out)) != tc.WantTag {
					t.Fatalf("resolution = %q, err=%v, want %s", out, err, tc.WantTag)
				}
				if requests.Load() != 1 {
					t.Fatalf("requests=%d, want 1", requests.Load())
				}
			})
		}
	}
}

func TestUnixCLIChannelPaginationAndBounds(t *testing.T) {
	for _, parser := range []string{"jq", "python3"} {
		for _, scenario := range []struct {
			name     string
			want     string
			requests int32
		}{
			{"higher-second-page", "v0.10.0", 2},
			{"first-page-newer", "v1.0.0", 2},
			{"second-page-fails", "", 2},
			{"full-tenth-page", "", 10},
			{"oversized-page", "", 1},
			{"malformed-page", "", 1},
			{"concatenated-json", "", 1},
			{"non-array", "", 1},
			{"too-many-entries", "", 1},
			{"newline-tag", "", 1},
		} {
			t.Run(parser+"/"+scenario.name, func(t *testing.T) {
				var requests atomic.Int32
				var server *httptest.Server
				server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if r.URL.Path != "/repos/Ricardo-M-L/metis/releases" || r.URL.Query().Get("per_page") != "100" || page < 1 {
						t.Errorf("unexpected request %s", r.URL)
						http.NotFound(w, r)
						return
					}
					switch scenario.name {
					case "oversized-page":
						fmt.Fprint(w, strings.Repeat(" ", 8*1024*1024+1))
						return
					case "malformed-page":
						fmt.Fprint(w, `[{`)
						return
					case "concatenated-json":
						fmt.Fprint(w, `[] []`)
						return
					case "non-array":
						fmt.Fprint(w, `{}`)
						return
					case "newline-tag":
						_ = json.NewEncoder(w).Encode([]any{unixCLIRelease("v0.4.51\n", server.URL)})
						return
					case "second-page-fails":
						if page == 2 {
							http.Error(w, "rate limited", http.StatusTooManyRequests)
							return
						}
					}
					releases := []any{unixCLIRelease("v0.4.51", server.URL)}
					if scenario.name == "first-page-newer" && page == 1 {
						releases[0] = unixCLIRelease("v1.0.0", server.URL)
					}
					if page == 1 || scenario.name == "full-tenth-page" {
						for len(releases) < 100 {
							releases = append(releases, map[string]any{"tag_name": "desktop-placeholder"})
						}
					} else {
						releases[0] = unixCLIRelease("v0.10.0", server.URL)
					}
					if scenario.name == "too-many-entries" {
						releases = append(releases, map[string]any{})
					}
					_ = json.NewEncoder(w).Encode(releases)
				}))
				server.Start()
				defer server.Close()
				out, err := unixCLIResolver(t, parser, server.URL, server.URL, "").CombinedOutput()
				if scenario.want == "" {
					if err == nil {
						t.Fatalf("unexpected success: %s", out)
					}
				} else if err != nil || strings.TrimSpace(string(out)) != scenario.want {
					t.Fatalf("resolution = %q, err=%v, want %s", out, err, scenario.want)
				}
				if requests.Load() != scenario.requests {
					t.Fatalf("requests=%d, want %d; output=%s", requests.Load(), scenario.requests, out)
				}
			})
		}
	}
}

func TestUnixCLIChannelNoJSONParser(t *testing.T) {
	for _, tag := range []string{"latest", "v0.4.51"} {
		t.Run(tag, func(t *testing.T) {
			cmd := unixCLIResolver(t, "none", "http://127.0.0.1:1", "http://127.0.0.1:1", "")
			cmd.Env = append(withoutEnv(cmd.Env, "METIS_VERSION"), "METIS_VERSION="+tag)
			out, err := cmd.CombinedOutput()
			if tag == "latest" {
				if err == nil || !strings.Contains(string(out), "requires jq or python3") {
					t.Fatalf("missing parser not reported: err=%v, output=%s", err, out)
				}
			} else if err != nil || strings.TrimSpace(string(out)) != tag {
				t.Fatalf("pinned version added parser dependency: err=%v, output=%s", err, out)
			}
		})
	}
}

func TestUnixCLIChannelMetadataBudget(t *testing.T) {
	for _, parser := range []string{"jq", "python3"} {
		t.Run(parser, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				select {
				case <-time.After(850 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				// Every full page demands another request. The shared budget must
				// stop traversal, rather than restarting a timeout for each page.
				fmt.Fprint(w, "["+strings.Repeat("{},", 99)+"{}]")
			}))
			defer server.Close()
			cmd := unixCLIResolver(t, parser, server.URL, server.URL, "")
			const original = "readonly METIS_RELEASE_METADATA_SECONDS=30"
			if !strings.Contains(cmd.Args[2], original) {
				t.Fatal("production metadata budget changed; update the bounded test deliberately")
			}
			// Shorten only the constant in the isolated function copy; execute
			// exactly the production budget accounting and network code.
			cmd.Args[2] = strings.Replace(cmd.Args[2], original, "readonly METIS_RELEASE_METADATA_SECONDS=2", 1)
			started := time.Now()
			out, err := cmd.CombinedOutput()
			if elapsed := time.Since(started); err == nil || elapsed > 4*time.Second || requests.Load() >= 5 {
				t.Fatalf("metadata budget not enforced: elapsed=%s, err=%v, requests=%d, output=%s", elapsed, err, requests.Load(), out)
			}
		})
	}
}

func TestUnixCLIChannelLatestRefusesInstalledDowngrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash installer is for macOS and Linux")
	}
	for _, layout := range []string{"flat", "managed"} {
		for _, current := range []string{"v0.4.53", "v0.4.53+local.abcdef", "v0.10.0", "v1.0.0-rc.1", "dev"} {
			t.Run(layout+"/"+current, func(t *testing.T) {
				var archives atomic.Int32
				var server *httptest.Server
				server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/repos/Ricardo-M-L/metis/releases" {
						_ = json.NewEncoder(w).Encode([]any{unixCLIRelease("v0.4.52", server.URL)})
						return
					}
					archives.Add(1)
					http.Error(w, "archive must not be requested", http.StatusInternalServerError)
				}))
				server.Start()
				defer server.Close()
				base, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				installDir := filepath.Join(base, "bin")
				if err := os.MkdirAll(installDir, 0o755); err != nil {
					t.Fatal(err)
				}
				launcher := filepath.Join(installDir, "metis")
				binaryPath := launcher
				if layout == "managed" {
					binaryPath = filepath.Join(base, "share", "metis", "versions", strings.TrimPrefix(current, "v"), "metis")
					if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(binaryPath, launcher); err != nil {
						t.Fatal(err)
					}
				}
				lockCheck := ""
				if layout == "managed" {
					lockCheck = "test -f '" + filepath.Join(base, "share", "metis", "locks", "install.lock.d", "owner.json") + "' || exit 17\n"
				}
				binary := []byte("#!/bin/sh\n" + lockCheck + "echo '" + current + " (Metis)'\n")
				if err := os.WriteFile(binaryPath, binary, 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("bash", "install.sh")
				cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
					"METIS_GITHUB_API_BASE="+server.URL, "METIS_GITHUB_WEB_BASE="+server.URL,
					"METIS_INSTALL_DIR="+installDir, "METIS_VERSION=latest", "METIS_REPO=Ricardo-M-L/metis")
				out, err := cmd.CombinedOutput()
				wantError := "refusing default CLI downgrade"
				if current == "dev" {
					wantError = "cannot compare installed CLI version"
				}
				if err == nil || archives.Load() != 0 || !strings.Contains(string(out), wantError) {
					t.Fatalf("default downgrade not blocked: err=%v, archives=%d, output=%s", err, archives.Load(), out)
				}
				got, err := os.ReadFile(binaryPath)
				if err != nil || string(got) != string(binary) {
					t.Fatalf("existing binary changed: err=%v, got=%q", err, got)
				}
				if layout == "managed" {
					if target, err := os.Readlink(launcher); err != nil || target != binaryPath {
						t.Fatalf("launcher changed: target=%q, err=%v", target, err)
					}
				}
			})
		}
	}
}

func TestUnixCLIChannelSameVersionAndExplicitPinStillDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash installer is for macOS and Linux")
	}
	for _, tc := range []struct{ current, request string }{
		{"v0.4.52", "latest"},
		{"v0.4.52+local.abcdef", "latest"},
		{"v0.4.53", "v0.4.52"},
		{"dev", "v0.4.52"},
	} {
		t.Run(tc.current+"/"+tc.request, func(t *testing.T) {
			var archives atomic.Int32
			var server *httptest.Server
			server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/Ricardo-M-L/metis/releases" {
					_ = json.NewEncoder(w).Encode([]any{unixCLIRelease("v0.4.52", server.URL)})
					return
				}
				archives.Add(1)
				http.Error(w, "test stops at archive request", http.StatusInternalServerError)
			}))
			server.Start()
			defer server.Close()
			installDir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(installDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(installDir, "metis"), []byte("#!/bin/sh\necho '"+tc.current+" (Metis)'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "install.sh")
			cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
				"METIS_GITHUB_API_BASE="+server.URL, "METIS_GITHUB_WEB_BASE="+server.URL,
				"METIS_INSTALL_DIR="+installDir, "METIS_VERSION="+tc.request, "METIS_REPO=Ricardo-M-L/metis")
			out, err := cmd.CombinedOutput()
			if err == nil || archives.Load() != 1 || !strings.Contains(string(out), "could not download") {
				t.Fatalf("same-version/pinned installation changed: err=%v, archives=%d, output=%s", err, archives.Load(), out)
			}
		})
	}
}
