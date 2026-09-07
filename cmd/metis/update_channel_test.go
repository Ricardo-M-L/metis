package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/update"
	"github.com/Ricardo-M-L/metis/internal/version"
)

type updateChannelTransport func(*http.Request) (*http.Response, error)

func (f updateChannelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Intercept the real release-discovery call, not the command's decision logic.
// No request leaves the test and no downloadable archive is ever provided.
func mockUpdateChannel(t *testing.T, running, candidate string) *int {
	t.Helper()
	t.Setenv("METIS_GITHUB_TOKEN", "test-token-never-sent")
	t.Setenv("METIS_REPO", "Ricardo-M-L/metis")
	t.Setenv("GOBIN", t.TempDir())
	oldVersion, oldTransport := version.Version, http.DefaultTransport
	version.Version = running
	t.Cleanup(func() {
		version.Version, http.DefaultTransport = oldVersion, oldTransport
	})
	assets := make([]map[string]any, 0, 12)
	for _, platform := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-arm64", "windows-amd64"} {
		ext := ".tar.gz"
		if strings.HasPrefix(platform, "windows-") {
			ext = ".zip"
		}
		for _, suffix := range []string{"", ".sha256"} {
			name := "metis-" + platform + ext + suffix
			assets = append(assets, map[string]any{
				"id": len(assets) + 1, "name": name, "size": 1, "state": "uploaded",
				"browser_download_url": "https://github.com/Ricardo-M-L/metis/releases/download/" + candidate + "/" + name,
			})
		}
	}
	release := map[string]any{
		"tag_name": candidate, "draft": false, "prerelease": false, "assets": assets,
		"html_url": "https://github.com/Ricardo-M-L/metis/releases/tag/" + candidate,
	}
	downloads := 0
	http.DefaultTransport = updateChannelTransport(func(r *http.Request) (*http.Response, error) {
		var payload any
		switch r.URL.Path {
		case "/repos/Ricardo-M-L/metis/releases":
			payload = []any{release}
		case "/repos/Ricardo-M-L/metis/releases/latest":
			// Also reproduce the old command bug before the channel migration.
			payload = release
		default:
			downloads++
			return nil, fmt.Errorf("test blocks all downloads")
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Request: r,
			Body: io.NopCloser(bytes.NewReader(body)),
		}, nil
	})
	return &downloads
}

func captureUpdateOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "update-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	runErr := run()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(output), runErr
}

func TestCmdUpdateCheckExplainsRunningVersionAheadOfChannel(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.47")
	output, err := captureUpdateOutput(t, func() error {
		return cmdUpdate(context.Background(), []string{"--check"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "newer than the currently available CLI stable release") || strings.Contains(output, "up to date") {
		t.Fatalf("ahead-of-channel output is misleading: %q", output)
	}
	if *downloads != 0 {
		t.Fatalf("check attempted %d downloads", *downloads)
	}
}

func TestCmdUpdateForceNeverDowngradesRunningVersion(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.47")
	output, err := captureUpdateOutput(t, func() error {
		return cmdUpdate(context.Background(), []string{"--force"})
	})
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("force must reject downgrade before installing: output=%q err=%v", output, err)
	}
	if *downloads != 0 || strings.Contains(output, "downloading") || strings.Contains(output, "installed") {
		t.Fatalf("force downgrade reached installation: downloads=%d output=%q", *downloads, output)
	}
	if strings.Contains(err.Error(), "test-token-never-sent") {
		t.Fatal("error exposed authentication token")
	}
}

func TestTryAutoInstallRejectsSecondLookupDowngrade(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.47")
	result, err := tryAutoInstall(context.Background(), "v0.4.52")
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("second lookup must reject downgrade: result=%+v err=%v", result, err)
	}
	if result.installed || result.notice != "" || *downloads != 0 {
		t.Fatalf("downgrade reached installation or notification: result=%+v downloads=%d", result, *downloads)
	}
}

func TestTryAutoInstallRejectsReleaseOlderThanAnnouncement(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.50", "v0.4.51")
	result, err := tryAutoInstall(context.Background(), "v0.4.52")
	if err == nil || !strings.Contains(err.Error(), "changed backwards") {
		t.Fatalf("stale second lookup must be rejected: result=%+v err=%v", result, err)
	}
	if result.installed || result.notice != "" || *downloads != 0 {
		t.Fatalf("stale lookup reached installation or notification: result=%+v downloads=%d", result, *downloads)
	}
}

func TestTryAutoInstallDoesNotReinstallRunningVersion(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.51")
	result, err := tryAutoInstall(context.Background(), "v0.4.51")
	if err != nil || result.installed || result.notice != "" || *downloads != 0 {
		t.Fatalf("same running version must be a no-op: result=%+v err=%v downloads=%d", result, err, *downloads)
	}
}

func TestCmdUpdateSameVersionNoForceDoesNotInstall(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.51")
	_, err := captureUpdateOutput(t, func() error { return cmdUpdate(context.Background(), nil) })
	if err != nil || *downloads != 0 {
		t.Fatalf("same version without force: err=%v downloads=%d", err, *downloads)
	}
}

func TestCmdUpdateForceAllowsSameVersionReinstall(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.51")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Treat the test executable as Go-managed, so the real command reaches
	// its path safety barrier without launching a binary probe or installer.
	t.Setenv("GOBIN", filepath.Dir(exe))
	output, err := captureUpdateOutput(t, func() error {
		return cmdUpdate(context.Background(), []string{"--force"})
	})
	if !errors.Is(err, update.ErrGoInstallManaged) || *downloads != 0 {
		t.Fatalf("same-version force did not reach the path safety barrier: err=%v downloads=%d", err, *downloads)
	}
	if strings.Contains(output, "installed") {
		t.Fatalf("blocked installer reported an installation: %q", output)
	}
}

func TestCheckUpdateTarget(t *testing.T) {
	for _, tc := range []struct {
		name, running, target, announced string
		wantError                        bool
	}{
		{"upgrade", "0.4.51", "v0.4.52", "", false},
		{"same version may be forced", "0.4.51", "v0.4.51", "", false},
		{"running newer", "0.4.51", "v0.4.47", "", true},
		{"running build metadata is not a downgrade", "0.4.51+local.abc", "v0.4.51", "", false},
		{"announced release unchanged", "0.4.50", "v0.4.51", "v0.4.51", false},
		{"announced release superseded", "0.4.50", "v0.4.52", "v0.4.51", false},
		{"announcement regressed", "0.4.50", "v0.4.51", "v0.4.52", true},
		{"announcement has build metadata", "0.4.50", "v0.4.51", "v0.4.51+build.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUpdateTarget(tc.running, tc.target, tc.announced)
			if (err != nil) != tc.wantError {
				t.Fatalf("checkUpdateTarget(%q, %q, %q) = %v, want error=%t", tc.running, tc.target, tc.announced, err, tc.wantError)
			}
		})
	}
}

// Ensure the fixture follows the actual updater's platform naming on the host.
func TestUpdateChannelFixtureIncludesHostTarget(t *testing.T) {
	mockUpdateChannel(t, "0.4.51", "v0.4.52")
	release, err := update.Latest(context.Background(), "test-token-never-sent")
	if err != nil {
		t.Fatal(err)
	}
	if release.TagName != "v0.4.52" {
		t.Fatalf("fixture selected %q", release.TagName)
	}
}
