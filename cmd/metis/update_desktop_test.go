package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/update"
)

func TestCmdUpdateDesktopManagedRejectsBeforeAnyRemoteCall(t *testing.T) {
	for _, args := range [][]string{nil, {"--force"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "1")
			t.Setenv("METIS_GITHUB_TOKEN", "test-token-never-sent")
			calls := 0
			old := http.DefaultTransport
			http.DefaultTransport = updateChannelTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("blocked unexpected remote request")
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			output, err := captureUpdateOutput(t, func() error { return cmdUpdate(context.Background(), args) })
			if !errors.Is(err, update.ErrDesktopManaged) || calls != 0 {
				t.Fatalf("err=%v remote calls=%d; want Desktop-owned error before any network", err, calls)
			}
			if strings.Contains(output, "installed") {
				t.Fatalf("blocked Desktop runtime reported install: %q", output)
			}
		})
	}
}

func TestTryAutoInstallDesktopManagedIsErrorAndNeverRequestsRelease(t *testing.T) {
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "1")
	t.Setenv("METIS_GITHUB_TOKEN", "test-token-never-sent")
	calls := 0
	old := http.DefaultTransport
	http.DefaultTransport = updateChannelTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("blocked unexpected remote request")
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	result, err := tryAutoInstall(context.Background(), "v99.0.0")
	if !errors.Is(err, update.ErrDesktopManaged) || calls != 0 || result.installed || result.notice != "" {
		t.Fatalf("result=%+v err=%v remote calls=%d; want guarded error and no success", result, err, calls)
	}
}

func TestCmdUpdateDesktopManagedCheckStillAllowed(t *testing.T) {
	downloads := mockUpdateChannel(t, "0.4.51", "v0.4.52")
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "1")
	output, err := captureUpdateOutput(t, func() error { return cmdUpdate(context.Background(), []string{"--check"}) })
	if err != nil || *downloads != 0 || !strings.Contains(output, "update available") || !strings.Contains(output, "update Metis Desktop") || strings.Contains(output, "run `metis update`") {
		t.Fatalf("Desktop check should remain read-only: output=%q err=%v downloads=%d", output, err, *downloads)
	}
}

func TestCmdUpdateDesktopManagedPathsBlockWithoutMarker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("LocalAppData", filepath.Join(root, "cache"))
	t.Setenv("GOBIN", filepath.Join(root, "go-bin"))
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "")
	t.Setenv("METIS_INSTALL_DIR", "")
	t.Setenv("METIS_GITHUB_TOKEN", "test-token-never-sent")
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "metis"
	if filepath.Ext(executable) == ".exe" {
		name += ".exe"
	}
	// A hard link to this running test image cannot be unlinked on Windows.
	// Copy it first and let a short-lived helper own the mapped image; this
	// parent owns both aliases and removes them after the helper exits.
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "desktop-update-helper.exe")
	if err := os.WriteFile(helper, image, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "Metis.app", "Contents", "Resources", "bin", "metis"),
		filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("d", 64), name),
	} {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, helper, "-test.run=^TestCmdUpdateDesktopManagedAliasHelperProcess$", "-test.count=1")
			command.Env = append(os.Environ(), "METIS_UPDATE_DESKTOP_ALIAS_TEST="+path)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("Desktop update guard helper: %v: %s", err, output)
			}
			// This is also a native Windows image-handle regression: do not
			// defer deletion or retry it while a process still owns the alias.
			if err := os.Remove(path); err != nil {
				t.Fatalf("helper retained executable alias: %v", err)
			}
		})
	}
	if err := os.Remove(helper); err != nil {
		t.Fatalf("helper executable remains mapped after Wait: %v", err)
	}
}

func TestCmdUpdateDesktopManagedAliasHelperProcess(t *testing.T) {
	path := os.Getenv("METIS_UPDATE_DESKTOP_ALIAS_TEST")
	if path == "" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(executable, path); err != nil {
		t.Fatalf("create verified alias of owned helper image: %v", err)
	}
	oldArg, oldTransport := os.Args[0], http.DefaultTransport
	os.Args[0] = path
	calls := 0
	http.DefaultTransport = updateChannelTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("blocked unexpected remote request")
	})
	t.Cleanup(func() { os.Args[0], http.DefaultTransport = oldArg, oldTransport })
	if err := cmdUpdate(context.Background(), []string{"--force"}); !errors.Is(err, update.ErrDesktopManaged) || calls != 0 {
		t.Fatalf("manual Desktop path bypassed guard without marker: err=%v requests=%d", err, calls)
	}
	result, err := tryAutoInstall(context.Background(), "v99.0.0")
	if !errors.Is(err, update.ErrDesktopManaged) || calls != 0 || result.installed || result.notice != "" {
		t.Fatalf("auto Desktop path bypassed guard without marker: result=%+v err=%v requests=%d", result, err, calls)
	}
}
