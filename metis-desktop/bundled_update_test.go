package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallBundledUpdateSkipsCLIUpdateAndResolvesRuntimeAfterRestart(t *testing.T) {
	workDir := t.TempDir()
	appPath := filepath.Join(workDir, "METIS.app")
	installed := false
	restarted := false
	quit := false
	app := &App{
		workDir: workDir,
		resolveRuntime: func() (desktopRuntimeSelection, error) {
			return desktopRuntimeSelection{Path: "/old/release/runtime/metis", Bundled: true}, nil
		},
		runMetis: func(context.Context, string, []string, string) (string, string, error) {
			t.Fatal("bundled CLI must not self-update")
			return "", "", nil
		},
		resolveUpdatedMetis: func(string) (string, error) {
			t.Fatal("bundled CLI must not resolve an external stable launcher")
			return "", nil
		},
		checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
			return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: true, CanUpdate: true}, nil
		},
		desktopPath: func() (string, error) { return appPath, nil },
		installDesktopUpdate: func(_ context.Context, _, path string) (DesktopUpdateStatus, error) {
			if path != appPath {
				t.Fatalf("install path = %q, want %q", path, appPath)
			}
			installed = true
			return DesktopUpdateStatus{LatestVersion: "9.9.9", Installed: true}, nil
		},
		restartDesktop: func(path, workspace, cli string) error {
			if !installed || path != appPath || workspace != workDir || cli != "" {
				t.Fatalf("restart = installed:%v path:%q workspace:%q cli:%q", installed, path, workspace, cli)
			}
			restarted = true
			return nil
		},
		scheduleRestart: func(fn func()) { fn() },
		quit:            func(context.Context) { quit = true },
	}
	var progress []DesktopUpdateProgress
	status, err := app.installUpdateAndRestart(func(p DesktopUpdateProgress) { progress = append(progress, p) })
	if err != nil || !status.Installed || !status.Restarting || !restarted || !quit {
		t.Fatalf("bundled update = %+v, error:%v restarted:%v quit:%v", status, err, restarted, quit)
	}
	for _, p := range progress {
		if p.Phase == "cli" {
			t.Fatalf("bundled update advertised independent CLI update: %+v", p)
		}
	}
}

func TestInstallBundledUpdateStopsOnRuntimeOrInstallFailure(t *testing.T) {
	for _, stage := range []string{"runtime", "install"} {
		t.Run(stage, func(t *testing.T) {
			installed := false
			app := &App{
				resolveRuntime: func() (desktopRuntimeSelection, error) {
					if stage == "runtime" {
						return desktopRuntimeSelection{}, errors.New("runtime fixture rejected")
					}
					return desktopRuntimeSelection{Path: "/old/metis", Bundled: true}, nil
				},
				runMetis: func(context.Context, string, []string, string) (string, string, error) {
					t.Fatal("bundled CLI must not self-update")
					return "", "", nil
				},
				checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
					return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: true, CanUpdate: true}, nil
				},
				desktopPath: func() (string, error) { return "/Applications/METIS.app", nil },
				installDesktopUpdate: func(context.Context, string, string) (DesktopUpdateStatus, error) {
					installed = true
					return DesktopUpdateStatus{}, errors.New("install fixture rejected")
				},
				restartDesktop: func(string, string, string) error {
					t.Fatal("failed update must not restart Desktop")
					return nil
				},
				scheduleRestart: func(func()) { t.Fatal("failed update must not schedule a quit") },
				quit:            func(context.Context) { t.Fatal("failed update must not quit Desktop") },
			}
			if _, err := app.InstallUpdateAndRestart(); err == nil || !strings.Contains(err.Error(), stage+" fixture rejected") {
				t.Fatalf("%s failure = %v", stage, err)
			}
			if installed != (stage == "install") {
				t.Fatalf("installation attempted after %s failure: %v", stage, installed)
			}
		})
	}
}

func TestInstallBundledUpdateRequiresRestartCapabilitiesBeforeInstalling(t *testing.T) {
	for _, missing := range []string{"restart", "scheduler", "quit"} {
		t.Run(missing, func(t *testing.T) {
			app := &App{
				resolveRuntime: func() (desktopRuntimeSelection, error) {
					return desktopRuntimeSelection{Path: "/old/metis", Bundled: true}, nil
				},
				checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
					return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: true, CanUpdate: true}, nil
				},
				desktopPath: func() (string, error) { return "/Applications/METIS.app", nil },
				installDesktopUpdate: func(context.Context, string, string) (DesktopUpdateStatus, error) {
					t.Fatal("cannot install or drain a backend when restart is unavailable")
					return DesktopUpdateStatus{}, nil
				},
				restartDesktop:  func(string, string, string) error { return nil },
				scheduleRestart: func(func()) {},
				quit:            func(context.Context) {},
			}
			switch missing {
			case "restart":
				app.restartDesktop = nil
			case "scheduler":
				app.scheduleRestart = nil
			case "quit":
				app.quit = nil
			}
			if _, err := app.InstallUpdateAndRestart(); err == nil || !strings.Contains(err.Error(), "restart is unavailable") {
				t.Fatalf("missing %s = %v", missing, err)
			}
		})
	}
}

func TestInstallBundledUpdateRecoversBackendAfterInstallOrRestartFailure(t *testing.T) {
	for _, stage := range []string{"install", "restart"} {
		for _, recoveryFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovery_fails_%v", stage, recoveryFails), func(t *testing.T) {
				originalErr := errors.New(stage + " fixture failure")
				recoveryErr := errors.New("recovery fixture failure")
				appPath := filepath.Join(t.TempDir(), "METIS.app")
				workspace := t.TempDir()
				restarts, scheduled, quits := 0, 0, 0
				app := &App{
					workDir: workspace,
					resolveRuntime: func() (desktopRuntimeSelection, error) {
						return desktopRuntimeSelection{Path: "/old/runtime/metis", Bundled: true}, nil
					},
					checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
						return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: true, CanUpdate: true}, nil
					},
					desktopPath: func() (string, error) { return appPath, nil },
					restartDesktop: func(path, dir, binary string) error {
						if path != appPath || dir != workspace || binary != "" {
							t.Fatalf("recovery restart = path:%q workspace:%q runtime:%q", path, dir, binary)
						}
						restarts++
						if stage == "restart" && restarts == 1 {
							return originalErr
						}
						if recoveryFails {
							return recoveryErr
						}
						return nil
					},
					scheduleRestart: func(fn func()) { scheduled++; fn() },
					quit:            func(context.Context) { quits++ },
				}
				app.installDesktopUpdate = func(context.Context, string, string) (DesktopUpdateStatus, error) {
					app.webuiMu.Lock()
					app.webuiUpdating = true
					app.webuiUpdateNeedsRestart = true
					app.webuiMu.Unlock()
					if stage == "install" {
						return DesktopUpdateStatus{}, originalErr
					}
					return DesktopUpdateStatus{Installed: true, LatestVersion: "9.9.9"}, nil
				}
				status, err := app.InstallUpdateAndRestart()
				if !errors.Is(err, originalErr) {
					t.Fatalf("lost original %s error: %v", stage, err)
				}
				wantRestarts := 1
				if stage == "restart" {
					wantRestarts = 2
				}
				if restarts != wantRestarts {
					t.Fatalf("restart attempts = %d, want %d", restarts, wantRestarts)
				}
				if recoveryFails {
					if !errors.Is(err, recoveryErr) || status.Restarting || scheduled != 0 || quits != 0 {
						t.Fatalf("failed recovery = %+v error:%v scheduled:%d quits:%d", status, err, scheduled, quits)
					}
					return
				}
				if !status.Restarting || scheduled != 1 || quits != 1 {
					t.Fatalf("backend recovery = %+v error:%v scheduled:%d quits:%d", status, err, scheduled, quits)
				}
			})
		}
	}
}

func TestDesktopCandidateRequiresBundledRuntimeBeforeCodesign(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("simulated macOS candidate uses POSIX executable permissions")
	}
	bundle := filepath.Join(t.TempDir(), "METIS.app")
	writeTestDesktopBundle(t, bundle, "1.2.0", "desktop-fixture")
	marker := fakeBundledUpdateCodesign(t)

	err := validateDesktopCandidate(bundle, "1.2.0", "darwin")
	if err == nil {
		t.Fatal("candidate without its bundled CLI and manifest was accepted")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("codesign ran before bundled runtime validation: %v (validation: %v)", statErr, err)
	}
}

func TestDesktopCandidateValidatesBundledManifestBeforeCodesign(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("simulated macOS candidate uses POSIX executable permissions")
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"valid", nil},
		{"version mismatch", func(m map[string]any) { m["version"] = "1.1.0" }},
		{"platform mismatch", func(m map[string]any) { m["goos"] = "linux" }},
		{"architecture mismatch", func(m map[string]any) { m["goarch"] = "unsupported" }},
		{"hash mismatch", func(m map[string]any) { m["sha256"] = strings.Repeat("0", 64) }},
		{"size mismatch", func(m map[string]any) { m["size_bytes"] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := filepath.Join(t.TempDir(), "METIS.app")
			writeTestDesktopBundle(t, bundle, "1.2.0", "desktop-fixture")
			writeBundledUpdateRuntime(t, bundle, tc.change)
			marker := fakeBundledUpdateCodesign(t)
			err := validateDesktopCandidateForPlatform(bundle, "1.2.0", "darwin", "arm64")
			if tc.change == nil {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("valid runtime did not reach signature check: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "bundled runtime") {
				t.Fatalf("invalid %s accepted: %v", tc.name, err)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("codesign ran before %s was rejected: %v", tc.name, statErr)
			}
		})
	}
}

func TestDesktopUpdaterDefaultValidationRejectsMissingRuntimeWithoutActivation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("simulated macOS candidate uses POSIX executable permissions")
	}
	archive := makeDesktopZip(t, "1.2.0", "candidate")
	sum := sha256.Sum256(archive)
	assetName := "metis-desktop-darwin-universal.zip"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest":
			http.Redirect(w, r, server.URL+"/owner/repo/releases/tag/v1.2.0", http.StatusFound)
		case "/owner/repo/releases/tag/v1.2.0":
			w.WriteHeader(http.StatusOK)
		case "/owner/repo/releases/download/v1.2.0/" + assetName:
			_, _ = w.Write(archive)
		case "/owner/repo/releases/download/v1.2.0/" + assetName + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	appPath := filepath.Join(t.TempDir(), "METIS.app")
	writeTestDesktopBundle(t, appPath, "1.0.0", "current")
	marker := fakeBundledUpdateCodesign(t)
	u := desktopUpdater{webBase: server.URL, repo: "owner/repo", client: server.Client(), goos: "darwin", goarch: "arm64"}
	if _, err := u.Install(context.Background(), "1.0.0", appPath); err == nil || !strings.Contains(err.Error(), "bundled runtime") {
		t.Fatalf("install candidate without runtime = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(got) != "current" {
		t.Fatalf("current application changed after rejected runtime: %q, %v", got, err)
	}
	if _, err := os.Stat(appPath + ".previous"); !os.IsNotExist(err) {
		t.Fatalf("rejected candidate created a rollback: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("missing runtime reached codesign: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(appPath), ".metis-update-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("rejected update left staging directories: %v, %v", matches, err)
	}
}

func TestDesktopUpdaterDefaultValidationInstallsRuntimeWithDesktop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("simulated macOS candidate uses POSIX executable permissions")
	}
	bundle := filepath.Join(t.TempDir(), "METIS.app")
	writeTestDesktopBundle(t, bundle, "1.2.0", "candidate")
	writeBundledUpdateRuntime(t, bundle, nil)
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for _, relative := range []string{"Contents/Info.plist", "Contents/MacOS/metis-desktop", "Contents/Resources/bin/metis", "Contents/Resources/bin/metis-runtime.json"} {
		path := filepath.Join(bundle, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		h := &zip.FileHeader{Name: "METIS.app/" + relative, Method: zip.Deflate}
		h.SetMode(info.Mode())
		out, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := zipped.Bytes()
	sum := sha256.Sum256(archive)
	assetName := "metis-desktop-darwin-universal.zip"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest":
			http.Redirect(w, r, server.URL+"/owner/repo/releases/tag/v1.2.0", http.StatusFound)
		case "/owner/repo/releases/tag/v1.2.0":
			w.WriteHeader(http.StatusOK)
		case "/owner/repo/releases/download/v1.2.0/" + assetName:
			_, _ = w.Write(archive)
		case "/owner/repo/releases/download/v1.2.0/" + assetName + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	appPath := filepath.Join(t.TempDir(), "METIS.app")
	writeTestDesktopBundle(t, appPath, "1.0.0", "legacy-current")
	// A previous version predating bundled runtimes remains a valid rollback.
	writeTestDesktopBundle(t, appPath+".previous", "0.9.0", "legacy-previous")
	fakeBundledUpdateCodesign(t)
	u := desktopUpdater{webBase: server.URL, repo: "owner/repo", client: server.Client(), goos: "darwin", goarch: "arm64"}
	status, err := u.Install(context.Background(), "1.0.0", appPath)
	if err != nil || !status.Installed {
		t.Fatalf("bundled install = %+v, %v", status, err)
	}
	if _, err := verifyMacDesktopRuntime(appPath, "1.2.0", "arm64"); err != nil {
		t.Fatalf("installed Desktop lost its matching runtime: %v", err)
	}
	old, err := os.ReadFile(filepath.Join(appPath+".previous", "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(old) != "legacy-current" {
		t.Fatalf("legacy Desktop rollback = %q, %v", old, err)
	}
}

func TestRestartBundledDesktopOmitsPreviousRuntimeOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS restart handoff uses a POSIX shell")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "args")
	opener := filepath.Join(dir, "open")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$METIS_BUNDLED_RESTART_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command("/bin/sh", "-c", "exit 0")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parentPID := parent.Process.Pid
	if err := parent.Wait(); err != nil {
		t.Fatal(err)
	}
	appPath := "/Applications/METIS $(unused).app"
	workspace := "/tmp/work space;literal"
	cmd, err := restartDesktopCommand("darwin", parentPID, appPath, workspace, "", opener)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "METIS_BUNDLED_RESTART_ARGS="+marker)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	want := "-n\n-a\n" + appPath + "\n--args\n--workspace\n" + workspace + "\n"
	if err != nil || string(got) != want {
		t.Fatalf("bundled restart args = %q, %v; want %q", got, err, want)
	}
	linux, err := restartDesktopCommand("linux", parentPID, "/opt/metis-desktop", workspace, "", "")
	if err != nil || len(linux.Args) != 3 || linux.Args[1] != "--workspace" || linux.Args[2] != workspace {
		t.Fatalf("Linux bundled restart args = %v, %v", linux, err)
	}
}

func TestDesktopRestartRebuildsBundleIdentityAndPreservesExternalOverride(t *testing.T) {
	t.Setenv("METIS_DESKTOP_APP", "/old/METIS.app")
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "/old/runtime/metis")
	t.Setenv("METIS_BIN", "/custom/external/metis")
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			cmd, err := restartDesktopCommand(goos, os.Getpid(), "/new/METIS.app", "/workspace", "", "")
			if err != nil {
				t.Fatal(err)
			}
			foundOverride := false
			for _, entry := range cmd.Environ() {
				if strings.HasPrefix(entry, "METIS_DESKTOP_APP=") || strings.HasPrefix(entry, "METIS_DESKTOP_BUNDLED_RUNTIME=") {
					t.Fatalf("restart inherited obsolete native runtime identity: %q", entry)
				}
				if entry == "METIS_BIN=/custom/external/metis" {
					foundOverride = true
				}
			}
			if !foundOverride {
				t.Fatal("restart discarded explicit external CLI override")
			}
		})
	}
}

func writeBundledUpdateRuntime(t *testing.T, bundle string, change func(map[string]any)) {
	t.Helper()
	binDir := filepath.Join(bundle, "Contents", "Resources", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Opaque fixture bytes deliberately cannot execute; runtime validation
	// must inspect the manifest and file instead of invoking the candidate.
	data := []byte("bundled-cli-fixture-must-never-execute")
	if err := os.WriteFile(filepath.Join(binDir, "metis"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	manifest := map[string]any{"schema_version": 1, "version": "1.2.0", "goos": "darwin", "goarch": "arm64", "sha256": hex.EncodeToString(sum[:]), "size_bytes": len(data)}
	if change != nil {
		change(manifest)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "metis-runtime.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeBundledUpdateCodesign(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "codesign-called")
	// The fixture executable only records that signature verification was
	// reached. No candidate Desktop or CLI executable is ever launched.
	script := "#!/bin/sh\nprintf verified > \"$METIS_BUNDLED_UPDATE_CODESIGN_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "codesign"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_BUNDLED_UPDATE_CODESIGN_MARKER", marker)
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	return marker
}
