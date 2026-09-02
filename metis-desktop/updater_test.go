package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRestartDesktopCommandWaitsForParentExitBeforeLaunching(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("restart handoff test requires POSIX process signals")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "restart-args.txt")
	opener := filepath.Join(dir, "open")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$METIS_RESTART_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	parent := exec.Command("/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do /bin/sleep 1; done")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if parent.Process != nil {
			_ = parent.Process.Kill()
			_ = parent.Wait()
		}
	}()

	cmd, err := restartDesktopCommand(
		"darwin",
		parent.Process.Pid,
		"/Applications/Metis Test.app",
		"/tmp/work space",
		"/tmp/metis bin",
		opener,
	)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "METIS_RESTART_TEST_MARKER="+marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("new Desktop launched before old process exited: stat error = %v", err)
	}

	if err := parent.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := parent.Wait(); err != nil {
		t.Fatal(err)
	}
	parent.Process = nil

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("restart helper failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart helper did not launch Desktop after old process exited")
	}
	cmd.Process = nil

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := "-n\n-a\n/Applications/Metis Test.app\n--args\n--workspace\n/tmp/work space\n--metis-bin\n/tmp/metis bin\n"
	if string(got) != want {
		t.Fatalf("restart arguments = %q, want %q", got, want)
	}
}

func TestDesktopAssetName(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "metis-desktop-darwin-universal.zip"},
		{"darwin", "amd64", "metis-desktop-darwin-universal.zip"},
		{"linux", "amd64", "metis-desktop-linux-amd64.tar.gz"},
		{"windows", "amd64", "metis-desktop-windows-amd64.zip"},
		{"linux", "arm64", ""},
	} {
		got, _ := desktopAssetName(tc.goos, tc.goarch)
		if got != tc.want {
			t.Errorf("desktopAssetName(%q,%q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestDesktopVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            bool
	}{
		{"0.4.28", "v0.4.29", true},
		{"0.4.28", "v0.5.0", true},
		{"0.4.28", "v1.0.0", true},
		{"0.4.28", "v0.4.28", false},
		{"0.4.29", "v0.4.28", false},
	} {
		if got := desktopVersionNewer(tc.current, tc.latest); got != tc.want {
			t.Errorf("desktopVersionNewer(%q,%q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestDesktopUpdaterDoesNotAdvertiseUnsafeWindowsActivation(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest":
			http.Redirect(w, r, server.URL+"/owner/repo/releases/tag/v1.2.0", http.StatusFound)
		case "/owner/repo/releases/tag/v1.2.0":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	updater := desktopUpdater{webBase: server.URL, repo: "owner/repo", client: server.Client(), goos: "windows", goarch: "amd64"}
	status, err := updater.Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.CanUpdate || !strings.Contains(status.Message, "Windows") {
		t.Fatalf("Check() = %+v, want available but safely disabled Windows update", status)
	}
}

func TestDesktopUpdaterDownloadsVerifiesAndAtomicallyReplacesBundle(t *testing.T) {
	archive := makeDesktopZip(t, "1.2.0", "new-build")
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

	root := t.TempDir()
	appPath := filepath.Join(root, "Metis.app")
	writeTestDesktopBundle(t, appPath, "1.0.0", "old-build")
	updater := desktopUpdater{
		webBase: server.URL,
		repo:    "owner/repo",
		client:  server.Client(),
		goos:    "darwin",
		goarch:  "arm64",
		validate: func(path, version string) error {
			if version != "1.2.0" {
				return fmt.Errorf("version = %s", version)
			}
			_, err := os.Stat(filepath.Join(path, "Contents", "MacOS", "metis-desktop"))
			return err
		},
	}

	status, err := updater.Check(context.Background(), "1.0.0")
	if err != nil || !status.Available || !status.CanUpdate || status.LatestVersion != "1.2.0" {
		t.Fatalf("Check() = %+v, %v", status, err)
	}
	status, err = updater.Install(context.Background(), "1.0.0", appPath)
	if err != nil || !status.Installed {
		t.Fatalf("Install() = %+v, %v", status, err)
	}
	newData, err := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(newData) != "new-build" {
		t.Fatalf("new executable = %q, %v", newData, err)
	}
	oldData, err := os.ReadFile(filepath.Join(appPath+".previous", "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(oldData) != "old-build" {
		t.Fatalf("rollback executable = %q, %v", oldData, err)
	}
}

func TestDesktopUpdaterRejectsChecksumMismatchWithoutTouchingCurrentBundle(t *testing.T) {
	archive := makeDesktopZip(t, "1.2.0", "untrusted-build")
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
			fmt.Fprintf(w, "%064d  %s\n", 0, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	appPath := filepath.Join(root, "Metis.app")
	writeTestDesktopBundle(t, appPath, "1.0.0", "trusted-current-build")
	updater := desktopUpdater{webBase: server.URL, repo: "owner/repo", client: server.Client(), goos: "darwin", goarch: "arm64"}
	if _, err := updater.Install(context.Background(), "1.0.0", appPath); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Install() checksum error = %v", err)
	}
	current, err := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(current) != "trusted-current-build" {
		t.Fatalf("current bundle changed after rejected update: %q, %v", current, err)
	}
	if _, err := os.Stat(appPath + ".previous"); !os.IsNotExist(err) {
		t.Fatalf("rollback path created before verification completed: %v", err)
	}
	lockPath := filepath.Join(root, "."+filepath.Base(appPath)+".metis-update.lock")
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("update lock remained after rejected update: %v", err)
	}
}

func TestExtractDesktopZipRejectsTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "bad")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractDesktopArchive(path, t.TempDir(), "darwin"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("traversal extraction error = %v", err)
	}
}

func TestDesktopUpdateLockExcludesConcurrentInstaller(t *testing.T) {
	appPath := filepath.Join(t.TempDir(), "Metis.app")
	release, err := acquireDesktopUpdateLock(appPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireDesktopUpdateLock(appPath); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second acquire error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	releaseAgain, err := acquireDesktopUpdateLock(appPath)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := releaseAgain(); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopRollbackRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "unrelated")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "Metis.app.previous")
	if err := os.Symlink(target, symlink); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating symlinks requires an elevated Windows runner: %v", err)
		}
		t.Fatal(err)
	}
	if err := removeDesktopRollback(symlink, "darwin"); err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("symlink rollback error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestDesktopRollbackRefusesArbitraryDirectory(t *testing.T) {
	rollback := filepath.Join(t.TempDir(), "Metis.app.previous")
	if err := os.Mkdir(rollback, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeDesktopRollback(rollback, "darwin"); err == nil || !strings.Contains(err.Error(), "unsafe bundle layout") {
		t.Fatalf("arbitrary rollback directory error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rollback, "keep.txt")); err != nil {
		t.Fatalf("arbitrary rollback directory was modified: %v", err)
	}
}

func TestDesktopRollbackRefusesUnrelatedExecutable(t *testing.T) {
	backup := filepath.Join(t.TempDir(), "metis-desktop.previous")
	if err := os.WriteFile(backup, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeDesktopRollback(backup, "linux"); err == nil ||
		(!strings.Contains(err.Error(), "ELF") && !strings.Contains(err.Error(), "not executable")) {
		t.Fatalf("unrelated executable error = %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("unrelated executable was removed: %v", err)
	}
}

func TestActivateDesktopCandidateReplacesVerifiedRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix execute bits for a simulated macOS bundle")
	}
	root := t.TempDir()
	appPath := filepath.Join(root, "Metis.app")
	backup := appPath + ".previous"
	candidate := filepath.Join(root, "candidate.app")
	writeTestDesktopBundle(t, appPath, "1.1.0", "current")
	writeTestDesktopBundle(t, backup, "1.0.0", "older-rollback")
	writeTestDesktopBundle(t, candidate, "1.2.0", "candidate")

	if err := activateDesktopCandidate(candidate, appPath, "darwin"); err != nil {
		t.Fatal(err)
	}
	gotCurrent, err := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(gotCurrent) != "candidate" {
		t.Fatalf("current executable = %q, %v", gotCurrent, err)
	}
	gotBackup, err := os.ReadFile(filepath.Join(backup, "Contents", "MacOS", "metis-desktop"))
	if err != nil || string(gotBackup) != "current" {
		t.Fatalf("rollback executable = %q, %v", gotBackup, err)
	}
}

func makeDesktopZip(t *testing.T, version, executable string) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "desktop.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	entries := map[string]string{
		"metis-desktop.app/Contents/Info.plist":          "<key>CFBundleShortVersionString</key><string>" + version + "</string>",
		"metis-desktop.app/Contents/MacOS/metis-desktop": executable,
	}
	for name, content := range entries {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestDesktopBundle(t *testing.T, path, version, executable string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := "<key>CFBundleIdentifier</key><string>com.metis.desktop</string>" +
		"<key>CFBundleShortVersionString</key><string>" + version + "</string>"
	if err := os.WriteFile(filepath.Join(path, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "Contents", "MacOS", "metis-desktop"), []byte(executable), 0o755); err != nil {
		t.Fatal(err)
	}
}
