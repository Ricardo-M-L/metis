package install_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPowerShellCLIChannelContract(t *testing.T) {
	script, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		`function Test-CompleteCLIRelease`,
		`function Read-CLIReleasePage`,
		`$MaxMetadataBytes = 8MB`,
		`$MaxReleasePages = 10`,
		`releases?per_page=100&page=$page`,
		`$cancellation.CancelAfter($MetadataTimeout)`,
		`$Cancellation.ThrowIfCancellationRequested()`,
		`$releaseCount -lt 100`,
		`Refusing to downgrade the installed CLI`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("PowerShell CLI channel lacks %q", want)
		}
	}
	if strings.Contains(text, "/releases/latest") {
		t.Error("PowerShell installer still resolves the shared Desktop latest channel")
	}
}

func powershellChannelCommand(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"powershell.exe", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("PowerShell is unavailable; resolver execution requires Windows PowerShell or pwsh")
	return ""
}

// Test only the actual resolver functions, never invoke installation or a CLI.
func runPowerShellChannel(t *testing.T, shell, api, web, token, timeout string) (string, error) {
	t.Helper()
	installer, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(installer), "$ErrorActionPreference =")
	end := strings.Index(string(installer), `if ($PSVersionTable.PSEdition -eq "Core"`)
	if start < 0 || end <= start {
		t.Fatal("installer function extraction boundaries missing")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := string(installer[start:end]) + "\n$Version='latest'\n$Repo='Ricardo-M-L/metis'\n$ApiBase=" + quote(api) + "\n$WebBase=" + quote(web) + "\n$Token=" + quote(token) + "\n"
	if timeout != "" {
		script += "$MetadataTimeout=[TimeSpan]::FromMilliseconds(" + timeout + ")\n"
	}
	script += "try { Resolve-ReleaseTag } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }\n"
	path := filepath.Join(t.TempDir(), "resolve.ps1")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path)
	cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func powershellCLIRelease(tag, web string) map[string]any {
	assets := []any{}
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			ext := ".tar.gz"
			if platform == "windows" {
				ext = ".zip"
			}
			name := "metis-" + platform + "-" + arch + ext
			for _, asset := range []string{name, name + ".sha256"} {
				assets = append(assets, map[string]any{"name": asset, "size": 1, "state": "uploaded", "browser_download_url": web + "/Ricardo-M-L/metis/releases/download/" + tag + "/" + asset})
			}
		}
	}
	return map[string]any{"tag_name": tag, "draft": false, "prerelease": false, "assets": assets}
}

func TestPowerShellCLIChannelSharedFixtures(t *testing.T) {
	shell := powershellChannelCommand(t)
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
	if len(fixture.Cases) == 0 {
		t.Fatal("no shared CLI channel cases")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/Ricardo-M-L/metis/releases" || r.URL.RawQuery != "per_page=100&page=1" {
					t.Errorf("unexpected metadata request %s", r.URL)
					http.Error(w, "unexpected route", http.StatusBadRequest)
					return
				}
				_, _ = w.Write(tc.Releases)
			}))
			defer server.Close()
			out, err := runPowerShellChannel(t, shell, server.URL, fixture.WebBase, "", "")
			if tc.WantTag == "" {
				if err == nil {
					t.Fatalf("accepted invalid release inventory: %s", out)
				}
			} else if err != nil || out != tc.WantTag {
				t.Fatalf("tag=%q err=%v, want %q", out, err, tc.WantTag)
			}
		})
	}
}

func TestPowerShellCLIChannelPaginationAndFailures(t *testing.T) {
	shell := powershellChannelCommand(t)
	for _, tc := range []struct {
		name  string
		token bool
		mode  string
		want  string
	}{
		{"anonymous_list51_ignores_latest47", false, "ok", "v0.4.51"},
		{"authenticated_same_channel", true, "ok", "v0.4.51"},
		{"base_urls_allow_trailing_slash", false, "slash", "v0.4.51"},
		{"highest_version_on_second_page", false, "paginate", "v0.4.52"},
		{"full_page_then_empty", false, "page_empty", "v0.4.51"},
		{"partial_page_then_error", false, "page_error", ""},
		{"partial_page_then_malformed", false, "page_invalid", ""},
		{"partial_page_then_rate_limit", false, "page_rate", ""},
		{"rate_limit_no_web_fallback", false, "rate", ""},
		{"oversized_metadata", false, "oversized", ""},
		{"page_over_100_releases", false, "too_many", ""},
		{"ten_full_pages_fail_closed", false, "full", ""},
		{"one_deadline_includes_body", false, "deadline", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			web := "https://github.com"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/repos/Ricardo-M-L/metis/releases" {
					t.Errorf("forbidden fallback request %s", r.URL)
					_ = json.NewEncoder(w).Encode(powershellCLIRelease("v0.4.47", web))
					return
				}
				wantAuth := ""
				if tc.token {
					wantAuth = "Bearer fixture-token"
				}
				if r.Header.Get("Authorization") != wantAuth {
					t.Error("incorrect authorization presence")
				}
				if r.URL.Query().Get("per_page") != "100" {
					t.Error("incorrect page size")
				}
				page := r.URL.Query().Get("page")
				if tc.mode == "rate" || tc.mode == "page_rate" && page == "2" {
					http.Error(w, "rate limited", http.StatusForbidden)
					return
				}
				if tc.mode == "page_error" && page == "2" {
					http.Error(w, "broken page", http.StatusBadGateway)
					return
				}
				if tc.mode == "page_invalid" && page == "2" {
					fmt.Fprint(w, `{}`)
					return
				}
				if tc.mode == "page_empty" && page == "2" {
					fmt.Fprint(w, `[]`)
					return
				}
				if tc.mode == "too_many" {
					releases := []any{}
					for len(releases) < 101 {
						releases = append(releases, powershellCLIRelease("v0.4.51", web))
					}
					_ = json.NewEncoder(w).Encode(releases)
					return
				}
				if tc.mode == "oversized" {
					fmt.Fprint(w, "[\""+strings.Repeat("a", 8*1024*1024)+"\"]")
					return
				}
				if tc.mode == "deadline" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(400 * time.Millisecond):
					}
				}
				if tc.mode == "full" || page == "1" && (strings.HasPrefix(tc.mode, "page") || tc.mode == "paginate" || tc.mode == "deadline") {
					releases := []any{powershellCLIRelease("v0.4.51", web)}
					for len(releases) < 100 {
						releases = append(releases, powershellCLIRelease("v0.0.1-rc.1", web))
					}
					_ = json.NewEncoder(w).Encode(releases)
					return
				}
				tag := "v0.4.51"
				if tc.mode == "paginate" || tc.mode == "deadline" {
					tag = "v0.4.52"
				}
				_ = json.NewEncoder(w).Encode([]any{powershellCLIRelease(tag, web), powershellCLIRelease("v0.4.47", web)})
			}))
			defer server.Close()
			token, timeout := "", ""
			if tc.token {
				token = "fixture-token"
			}
			if tc.mode == "deadline" {
				timeout = "650"
			}
			api := server.URL
			resolveWeb := web
			if tc.mode == "slash" {
				api += "/"
				resolveWeb += "/"
			}
			out, err := runPowerShellChannel(t, shell, api, resolveWeb, token, timeout)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unsafe metadata returned %q", out)
				}
			} else if err != nil || out != tc.want {
				t.Fatalf("tag=%q err=%v want %s", out, err, tc.want)
			}
			if strings.Contains(out, "fixture-token") {
				t.Error("credential leaked in output")
			}
			if tc.mode == "full" && requests.Load() != 10 {
				t.Errorf("requests=%d, want 10", requests.Load())
			}
		})
	}
}
