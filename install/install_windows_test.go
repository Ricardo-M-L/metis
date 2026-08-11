package install_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPowerShellInstallerHasManagedVersionLifecycle(t *testing.T) {
	script, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		`Join-Path $installRoot "versions"`,
		`Join-Path $installRoot "staging"`,
		`Join-Path $installRoot "locks"`,
		`current-version`,
		`ToUnixTimeSeconds()`,
		`FromUnixTimeSeconds($CreatedAt)`,
		`$VersionRetentionCount = 2`,
		`function Cleanup-ManagedVersions`,
		`function Migrate-FlatLauncher`,
		`function Get-ManagedVersionMatches`,
		`METIS_INSTALL_TEST_CRASH_AFTER`,
		`Refusing to overwrite managed version`,
		`Select-Object -Skip $VersionRetentionCount`,
		`Get-Process -Name "metis"`,
		`[System.IO.FileAttributes]::ReparsePoint`,
		`.metis.old.`,
		`[System.IO.File]::Replace`,
		`$MaxArchiveBytes = 128MB`,
		`$MaxExpandedBytes = 128MB`,
		`function Download-FileWithLimit`,
		`ContentLength`,
		`$entry.Length -gt $MaxExpandedBytes`,
		`[TimeSpan]::FromSeconds(30)`,
		`[TimeSpan]::FromSeconds(300)`,
		`Refusing to overwrite unverified existing launcher`,
		`version output has no Metis product marker`,
		`install.lock.d.pending.`,
		`install.reclaim.`,
		`$guardDir + ".pending." + $guardNonce`,
		`.retired.`,
		`function Read-JSONOwner`,
		`[System.IO.Directory]::Move($pendingDir, $lockDir)`,
		`function Assert-ManagedDirectory`,
		`Assert-ManagedDirectory $versionsDir`,
		`Assert-ManagedDirectory $stagingDir`,
		`Assert-ManagedDirectory $locksDir`,
		`Assert-ManagedDirectory $runningLocksDir`,
		`Move-Item -LiteralPath $destination -Destination $backup`,
		`Move-Item -LiteralPath $backup -Destination $destination`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("install.ps1 does not contain managed-update safeguard %q", want)
		}
	}
	if strings.Contains(text, "Expand-Archive") {
		t.Error("install.ps1 uses unbounded Expand-Archive")
	}
	if strings.Contains(text, "$IncompleteLockMaxAge") {
		t.Error("install.ps1 may reclaim an ownerless fixed lock by age")
	}
	if strings.Contains(text, `install.lock.pending.`) {
		t.Error("install.ps1 uses the obsolete pending-lock name")
	}
}

func TestPowerShellInstallerRecoversInterruptedActivation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell activation recovery test requires Windows")
	}
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skip("unsupported Windows test architecture")
	}
	artifact := "metis-windows-" + arch + ".zip"
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/Ricardo-M-L/metis/releases/download/")
		tag, asset, ok := strings.Cut(rest, "/")
		if !ok || (asset != artifact && asset != artifact+".sha256") {
			http.NotFound(w, r)
			return
		}
		archive := makeWindowsArchive(t, []byte("activation-binary-"+tag))
		if asset == artifact {
			_, _ = w.Write(archive)
			return
		}
		sum := sha256.Sum256(archive)
		_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), artifact)
	})

	installRoot := t.TempDir()
	installDir := filepath.Join(installRoot, "bin")
	run := func(version, crashAfter string) ([]byte, error) {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
			"-Version", version,
			"-InstallDir", installDir,
			"-Repo", "Ricardo-M-L/metis",
			"-ApiBase", server.URL,
			"-WebBase", server.URL,
			"-SkipVersionCheck",
		)
		cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN", "METIS_INSTALL_TEST_CRASH_AFTER")
		if crashAfter != "" {
			cmd.Env = append(cmd.Env, "METIS_INSTALL_TEST_CRASH_AFTER="+crashAfter)
		}
		return cmd.CombinedOutput()
	}
	if out, err := run("v1.0.0", ""); err != nil {
		t.Fatalf("seed managed install: %v\n%s", err, out)
	}

	for i, crashAfter := range []string{
		"marker-staged",
		"launcher-backed-up",
		"launcher-replaced",
		"marker-replaced",
	} {
		version := fmt.Sprintf("v1.0.%d", i+1)
		t.Run(crashAfter, func(t *testing.T) {
			if out, err := run(version, crashAfter); err == nil {
				t.Fatalf("fault injection %q did not terminate installer:\n%s", crashAfter, out)
			}
			if out, err := run(version, ""); err != nil {
				t.Fatalf("installer was not reentrant after %q: %v\n%s", crashAfter, err, out)
			}
			marker, err := os.ReadFile(filepath.Join(installRoot, "current-version"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := strings.TrimSpace(string(marker)), strings.TrimPrefix(version, "v"); got != want {
				t.Fatalf("current-version = %q, want %q", got, want)
			}
			launcher, err := os.ReadFile(filepath.Join(installDir, "metis.exe"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(launcher), "activation-binary-"+version; got != want {
				t.Fatalf("launcher = %q, want %q", got, want)
			}
		})
	}
}

func TestPowerShellInstallerRejectsReparseManagedRoots(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junction sentinel test requires Windows")
	}
	for _, target := range []string{"root", "versions", "staging", "locks", "running"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			external := t.TempDir()
			sentinel := filepath.Join(external, "sentinel.txt")
			if err := os.WriteFile(sentinel, []byte("do-not-touch"), 0o600); err != nil {
				t.Fatal(err)
			}
			managedRoot := filepath.Join(base, "Metis")
			if target == "root" {
				makeWindowsJunction(t, managedRoot, external)
			} else {
				if err := os.MkdirAll(managedRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(managedRoot, target)
				if target == "running" {
					locks := filepath.Join(managedRoot, "locks")
					if err := os.MkdirAll(locks, 0o755); err != nil {
						t.Fatal(err)
					}
					link = filepath.Join(locks, "running")
				}
				makeWindowsJunction(t, link, external)
			}

			cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
				"-Version", "v9.9.9",
				"-InstallDir", filepath.Join(managedRoot, "bin"),
				"-Repo", "Ricardo-M-L/metis",
				"-WebBase", "http://127.0.0.1:1",
				"-SkipVersionCheck",
			)
			cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("installer accepted %s reparse root:\n%s", target, out)
			}
			if !strings.Contains(strings.ToLower(string(out)), "reparse") {
				t.Fatalf("installer failed for the wrong reason: %v\n%s", err, out)
			}
			entries, err := os.ReadDir(external)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "sentinel.txt" {
				t.Fatalf("installer mutated external junction target: %v", entries)
			}
			got, err := os.ReadFile(sentinel)
			if err != nil || string(got) != "do-not-touch" {
				t.Fatalf("external sentinel changed: data=%q err=%v", got, err)
			}
		})
	}
}

func TestPowerShellInstallerRejectsReparseLockAndMarkerState(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junction sentinel test requires Windows")
	}
	for _, target := range []string{"install-lock", "lock-owner", "current-version"} {
		t.Run(target, func(t *testing.T) {
			installRoot := t.TempDir()
			external := t.TempDir()
			sentinel := filepath.Join(external, "sentinel.txt")
			if err := os.WriteFile(sentinel, []byte("do-not-touch"), 0o600); err != nil {
				t.Fatal(err)
			}
			installDir := filepath.Join(installRoot, "bin")
			locksDir := filepath.Join(installRoot, "locks")
			switch target {
			case "install-lock":
				if err := os.MkdirAll(locksDir, 0o755); err != nil {
					t.Fatal(err)
				}
				makeWindowsJunction(t, filepath.Join(locksDir, "install.lock.d"), external)
			case "lock-owner":
				lockDir := filepath.Join(locksDir, "install.lock.d")
				if err := os.MkdirAll(lockDir, 0o755); err != nil {
					t.Fatal(err)
				}
				makeWindowsJunction(t, filepath.Join(lockDir, "owner.json"), external)
			case "current-version":
				versionDir := filepath.Join(installRoot, "versions", "1.0.0")
				if err := os.MkdirAll(versionDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(installDir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{filepath.Join(versionDir, "metis.exe"), filepath.Join(installDir, "metis.exe")} {
					if err := os.WriteFile(path, []byte("managed"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				makeWindowsJunction(t, filepath.Join(installRoot, "current-version"), external)
			}

			cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
				"-Version", "v9.9.9", "-InstallDir", installDir,
				"-Repo", "Ricardo-M-L/metis", "-WebBase", "http://127.0.0.1:1", "-SkipVersionCheck")
			cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(strings.ToLower(string(out)), "reparse") {
				t.Fatalf("installer did not fail closed for %s reparse state: err=%v\n%s", target, err, out)
			}
			got, err := os.ReadFile(sentinel)
			if err != nil || string(got) != "do-not-touch" {
				t.Fatalf("external sentinel changed: data=%q err=%v", got, err)
			}
		})
	}
}

func TestPowerShellInstallerSupportsAnonymousPublicRelease(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell installer integration test requires Windows")
	}
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skip("unsupported Windows test architecture")
	}
	artifact := "metis-windows-" + arch + ".zip"
	serveBadChecksum := false
	oversizedExpandedArchive := makeOversizedExpandedWindowsArchive(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	assertAnonymous := func(r *http.Request) {
		t.Helper()
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous request sent Authorization %q", got)
		}
	}
	mux.HandleFunc("/Ricardo-M-L/metis/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(r)
		http.Redirect(w, r, "/Ricardo-M-L/metis/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/tag/v9.9.9", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(r)
		_, _ = w.Write([]byte("test release"))
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(r)
		rest := strings.TrimPrefix(r.URL.Path, "/Ricardo-M-L/metis/releases/download/")
		tag, asset, ok := strings.Cut(rest, "/")
		if !ok || (asset != artifact && asset != artifact+".sha256") {
			http.NotFound(w, r)
			return
		}
		binary := []byte("test-windows-binary-" + tag)
		archive := makeWindowsArchive(t, binary)
		if tag == "v9.9.2" {
			archive = oversizedExpandedArchive
		}
		if asset == artifact {
			if tag == "v9.9.4" {
				w.Header().Set("Content-Length", strconv.FormatInt((128<<20)+1, 10))
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write(archive)
			return
		}
		sum := sha256.Sum256(archive)
		encoded := hex.EncodeToString(sum[:])
		if serveBadChecksum && tag == "v9.9.5" {
			encoded = strings.Repeat("0", 64)
		}
		_, _ = fmt.Fprintf(w, "%s  %s\n", encoded, artifact)
	})

	installRoot := t.TempDir()
	installDir := filepath.Join(installRoot, "bin")
	runInstaller := func(version string) ([]byte, error) {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
			"-Version", version,
			"-InstallDir", installDir,
			"-Repo", "Ricardo-M-L/metis",
			"-ApiBase", server.URL,
			"-WebBase", server.URL,
			"-SkipVersionCheck",
		)
		cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN")
		return cmd.CombinedOutput()
	}
	for attempt := 1; attempt <= 2; attempt++ {
		out, err := runInstaller("latest")
		if err != nil {
			t.Fatalf("anonymous PowerShell install attempt %d failed: %v\n%s", attempt, err, out)
		}
	}
	lockDir := filepath.Join(installRoot, "locks", "install.lock.d")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf(`{"pid":%d,"version":"9.9.8","created_at":%d,"nonce":"go-test"}`, os.Getpid(), time.Now().Unix())
	if err := os.WriteFile(filepath.Join(lockDir, "owner.json"), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.8"); err == nil {
		t.Fatalf("installer ignored a live shared install lock:\n%s", out)
	}
	if err := os.RemoveAll(lockDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(lockDir, old, old); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.8"); err == nil {
		t.Fatalf("installer reclaimed an ownerless fixed lock:\n%s", out)
	}
	pending, err := filepath.Glob(filepath.Join(installRoot, "locks", "install.lock.d.pending.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("failed lock acquisition leaked pending directories: %v", pending)
	}
	if err := os.RemoveAll(lockDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	deadOwner := fmt.Sprintf(`{"pid":2147483000,"version":"9.9.8","created_at":%d,"nonce":"dead-go-test"}`, time.Now().Add(-time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(lockDir, "owner.json"), []byte(deadOwner), 0o600); err != nil {
		t.Fatal(err)
	}
	reclaimGuard := filepath.Join(installRoot, "locks", "install.reclaim.dead-go-test.d")
	if err := os.MkdirAll(reclaimGuard, 0o755); err != nil {
		t.Fatal(err)
	}
	deadReclaimer := fmt.Sprintf(`{"pid":2147483000,"nonce":"dead-reclaimer","target_pid":2147483000,"target_nonce":"dead-go-test","created_at":%d}`, time.Now().Add(-time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(reclaimGuard, "owner.json"), []byte(deadReclaimer), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.8"); err != nil {
		t.Fatalf("installer could not recover a dead reclaim guard and lock: %v\n%s", err, out)
	}
	reclaim, err := filepath.Glob(filepath.Join(installRoot, "locks", "install.reclaim.dead-go-test.d"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaim) != 0 {
		t.Fatalf("successful dead-owner reclaim leaked its guard: %v", reclaim)
	}
	retiredGuard := reclaimGuard + ".retired.dead-reclaimer"
	if info, err := os.Stat(retiredGuard); err != nil || !info.IsDir() {
		t.Fatalf("dead reclaim guard did not leave its permanent ABA tombstone: info=%v err=%v", info, err)
	}

	runningLockDir := filepath.Join(installRoot, "locks", "running", "9.9.9")
	if err := os.MkdirAll(runningLockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runningOwner, err := json.Marshal(map[string]any{
		"pid":        os.Getpid(),
		"nonce":      "go-running-test",
		"version":    "9.9.9",
		"exec_path":  filepath.Join(installRoot, "versions", "9.9.9", "metis.exe"),
		"created_at": time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runningLockDir, fmt.Sprintf("%d.json", os.Getpid())), runningOwner, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v9.9.8", "v9.9.7", "v9.9.6"} {
		out, err := runInstaller(version)
		if err != nil {
			t.Fatalf("install %s failed: %v\n%s", version, err, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	unknownRunningEntry := filepath.Join(installRoot, "locks", "running", "unexpected.txt")
	if err := os.WriteFile(unknownRunningEntry, []byte("uncertain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.5"); err != nil {
		t.Fatalf("install with unknown running-lock entry: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(installRoot, "versions", "9.9.8", "metis.exe")); err != nil {
		t.Fatalf("unknown running-lock entry did not protect every version: %v", err)
	}
	if err := os.Remove(unknownRunningEntry); err != nil {
		t.Fatal(err)
	}
	unknownRunningDir := filepath.Join(runningLockDir, "unexpected")
	if err := os.MkdirAll(unknownRunningDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.5"); err != nil {
		t.Fatalf("install with unknown nested running-lock directory: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(installRoot, "versions", "9.9.8", "metis.exe")); err != nil {
		t.Fatalf("unknown nested running-lock directory did not protect every version: %v", err)
	}
	if err := os.RemoveAll(unknownRunningDir); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.6"); err != nil {
		t.Fatalf("restore current version after running-lock uncertainty test: %v\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(installDir, "metis.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("test-windows-binary-v9.9.6"); !bytes.Equal(got, want) {
		t.Fatalf("installed binary mismatch: got %q", got)
	}
	marker, err := os.ReadFile(filepath.Join(installRoot, "current-version"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(marker)), "9.9.6"; got != want {
		t.Fatalf("current version marker = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(installRoot, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, entry := range entries {
		if entry.IsDir() {
			versions = append(versions, entry.Name())
		}
	}
	sort.Strings(versions)
	if got, want := strings.Join(versions, ","), "9.9.5,9.9.6,9.9.7,9.9.9"; got != want {
		t.Fatalf("versions retained while 9.9.9 is running = %q, want %q", got, want)
	}
	if err := os.RemoveAll(runningLockDir); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstaller("v9.9.6"); err != nil {
		t.Fatalf("cleanup install after releasing running lock: %v\n%s", err, out)
	}
	entries, err = os.ReadDir(filepath.Join(installRoot, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	versions = versions[:0]
	for _, entry := range entries {
		if entry.IsDir() {
			versions = append(versions, entry.Name())
		}
	}
	sort.Strings(versions)
	if got, want := strings.Join(versions, ","), "9.9.5,9.9.6,9.9.7"; got != want {
		t.Fatalf("retained versions = %q, want %q", got, want)
	}

	serveBadChecksum = true
	if out, err := runInstaller("v9.9.5"); err == nil {
		t.Fatalf("installer accepted a bad checksum:\n%s", out)
	}
	got, err = os.ReadFile(filepath.Join(installDir, "metis.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("test-windows-binary-v9.9.6"); !bytes.Equal(got, want) {
		t.Fatalf("failed install changed existing binary: got %q", got)
	}
	if out, err := runInstaller("v9.9.4"); err == nil {
		t.Fatalf("installer accepted an oversized archive Content-Length:\n%s", out)
	}
	got, err = os.ReadFile(filepath.Join(installDir, "metis.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("test-windows-binary-v9.9.6"); !bytes.Equal(got, want) {
		t.Fatalf("oversized download changed existing binary: got %q", got)
	}
	if out, err := runInstaller("v9.9.2"); err == nil {
		t.Fatalf("installer expanded a binary larger than 128 MiB:\n%s", out)
	}
	got, err = os.ReadFile(filepath.Join(installDir, "metis.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("test-windows-binary-v9.9.6"); !bytes.Equal(got, want) {
		t.Fatalf("oversized expansion changed existing binary: got %q", got)
	}

	customRoot := t.TempDir()
	customInstallDir := filepath.Join(customRoot, "bin")
	if err := os.MkdirAll(customInstallDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customLauncher := filepath.Join(customInstallDir, "metis.exe")
	customContents := []byte("not-a-metis-launcher")
	if err := os.WriteFile(customLauncher, customContents, 0o755); err != nil {
		t.Fatal(err)
	}
	runCustomInstall := func() ([]byte, error) {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
			"-Version", "v9.9.3",
			"-InstallDir", customInstallDir,
			"-Repo", "Ricardo-M-L/metis",
			"-ApiBase", server.URL,
			"-WebBase", server.URL,
			"-SkipVersionCheck",
		)
		cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN")
		return cmd.CombinedOutput()
	}
	if out, err := runCustomInstall(); err == nil {
		t.Fatalf("installer overwrote an unverified custom launcher:\n%s", out)
	}
	got, err = os.ReadFile(customLauncher)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, customContents) {
		t.Fatalf("failed closed migration changed custom launcher: got %q", got)
	}
	fakeMetis := buildWindowsVersionHelper(t, "v8.8.8 (OtherProduct)")
	fakeContents, err := os.ReadFile(fakeMetis)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customLauncher, fakeContents, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runCustomInstall(); err == nil {
		t.Fatalf("installer accepted a non-Metis executable with a valid version field:\n%s", out)
	}
	got, err = os.ReadFile(customLauncher)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeContents) {
		t.Fatal("non-Metis executable was overwritten")
	}
}

func TestPowerShellInstallerReplacesRunningLauncherAndDefersCleanup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("running executable replacement requires Windows")
	}
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skip("unsupported Windows test architecture")
	}
	artifact := "metis-windows-" + arch + ".zip"
	binary := []byte("replacement-windows-binary")
	archive := makeWindowsArchive(t, binary)
	sum := sha256.Sum256(archive)

	mux := http.NewServeMux()
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/v1.2.3/"+artifact, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/v1.2.3/"+artifact+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), artifact)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	installRoot := t.TempDir()
	installDir := filepath.Join(installRoot, "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(installDir, "metis.exe")
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	oldVersionDir := filepath.Join(installRoot, "versions", "0.0.1")
	if err := os.MkdirAll(oldVersionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldVersionDir, "metis.exe"), contents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installRoot, "current-version"), []byte("0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(launcher, "-test.run=^TestWindowsRunningExecutableHelper$")
	helper.Env = append(os.Environ(), "METIS_WINDOWS_INSTALLER_HELPER=1")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "ready" {
		t.Fatalf("running executable helper did not become ready: %q", scanner.Text())
	}

	runInstaller := func() ([]byte, error) {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
			"-Version", "v1.2.3",
			"-InstallDir", installDir,
			"-Repo", "Ricardo-M-L/metis",
			"-ApiBase", server.URL,
			"-WebBase", server.URL,
			"-SkipVersionCheck",
		)
		cmd.Env = withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN")
		return cmd.CombinedOutput()
	}
	out, err := runInstaller()
	if err != nil {
		t.Fatalf("replace running executable: %v\n%s", err, out)
	}
	got, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("replacement launcher mismatch: got %d bytes", len(got))
	}
	oldLaunchers, err := filepath.Glob(filepath.Join(installDir, ".metis.old.*.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(oldLaunchers) == 0 {
		t.Fatal("running launcher backup was removed before its process exited")
	}

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = helper.Process.Wait()
	if out, err := runInstaller(); err != nil {
		t.Fatalf("cleanup install after helper exit: %v\n%s", err, out)
	}
	oldLaunchers, err = filepath.Glob(filepath.Join(installDir, ".metis.old.*.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(oldLaunchers) != 0 {
		t.Fatalf("stale launcher backups were not cleaned up: %v", oldLaunchers)
	}
}

func TestWindowsRunningExecutableHelper(t *testing.T) {
	if os.Getenv("METIS_WINDOWS_INSTALLER_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	time.Sleep(30 * time.Second)
}

func makeWindowsArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("metis.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func makeOversizedExpandedWindowsArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("metis.exe")
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for range 128 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func buildWindowsVersionHelper(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	program := fmt.Sprintf("package main\nimport (\"fmt\"; \"os\")\nfunc main() { if len(os.Args) > 1 && os.Args[1] == \"version\" { fmt.Println(%q); return }; os.Exit(2) }\n", output)
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "helper.exe")
	cmd := exec.Command("go", "build", "-o", binary, source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build Windows version helper: %v\n%s", err, out)
	}
	return binary
}

func makeWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction %q -> %q: %v\n%s", link, target, err, out)
	}
}
