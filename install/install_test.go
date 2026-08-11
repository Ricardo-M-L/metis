package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"strings"
	"testing"
	"time"
)

func TestInstallerSupportsAnonymousPublicRelease(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	target := runtime.GOOS + "-" + runtime.GOARCH
	artifact := "metis-" + target + ".tar.gz"
	binary := []byte("#!/bin/sh\necho 'v9.9.9 (test)'\n")

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "metis-" + target,
		Mode: 0o755,
		Size: int64(len(binary)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	sum := sha256.Sum256(archive.Bytes())
	sumFile := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), artifact)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	assertAnonymous := func(t *testing.T, r *http.Request) {
		t.Helper()
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous request sent Authorization header %q", got)
		}
	}
	mux.HandleFunc("/repos/Ricardo-M-L/metis/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "anonymous API rate limit exhausted", http.StatusForbidden)
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(t, r)
		http.Redirect(w, r, "/Ricardo-M-L/metis/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/tag/v9.9.9", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(t, r)
		fmt.Fprint(w, "test release")
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/v9.9.9/"+artifact, func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(t, r)
		_, _ = w.Write(archive.Bytes())
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/v9.9.9/"+artifact+".sha256", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(t, r)
		_, _ = w.Write([]byte(sumFile))
	})
	for _, tag := range []string{"v9.9.8", "v9.9.7"} {
		tag := tag
		mux.HandleFunc("/Ricardo-M-L/metis/releases/download/"+tag+"/"+artifact, func(w http.ResponseWriter, r *http.Request) {
			assertAnonymous(t, r)
			_, _ = w.Write(archive.Bytes())
		})
		mux.HandleFunc("/Ricardo-M-L/metis/releases/download/"+tag+"/"+artifact+".sha256", func(w http.ResponseWriter, r *http.Request) {
			assertAnonymous(t, r)
			if tag == "v9.9.7" {
				fmt.Fprintf(w, "%064d  %s\n", 0, artifact)
				return
			}
			_, _ = w.Write([]byte(sumFile))
		})
	}

	baseDir := t.TempDir()
	installDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	legacyLauncher := filepath.Join(installDir, "metis")
	legacyBinary := []byte("#!/bin/sh\necho 'v0.1.0 (Metis)'\n")
	if err := os.WriteFile(legacyLauncher, legacyBinary, 0o755); err != nil {
		t.Fatalf("write legacy flat launcher: %v", err)
	}
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
		"METIS_GITHUB_API_BASE="+server.URL,
		"METIS_GITHUB_WEB_BASE="+server.URL,
		"METIS_INSTALL_DIR="+installDir,
		"METIS_VERSION=latest",
		"METIS_REPO=Ricardo-M-L/metis",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("anonymous install failed: %v\n%s", err, out)
	}

	installed := filepath.Join(installDir, "metis")
	info, err := os.Lstat(installed)
	if err != nil {
		t.Fatalf("lstat installed launcher: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("installed launcher is not a symlink: %v", info.Mode())
	}

	versioned := filepath.Join(baseDir, "share", "metis", "versions", "9.9.9", "metis")
	versioned, err = filepath.EvalSymlinks(versioned)
	if err != nil {
		t.Fatalf("resolve expected versioned binary: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(installed)
	if err != nil {
		t.Fatalf("resolve installed launcher: %v", err)
	}
	if resolved != versioned {
		t.Fatalf("launcher target = %q, want %q", resolved, versioned)
	}

	got, err := os.ReadFile(versioned)
	if err != nil {
		t.Fatalf("read versioned binary: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed binary mismatch: got %q", got)
	}
	info, err = os.Stat(versioned)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
	legacyVersioned := filepath.Join(baseDir, "share", "metis", "versions", "0.1.0", "metis")
	gotLegacy, err := os.ReadFile(legacyVersioned)
	if err != nil {
		t.Fatalf("read migrated legacy binary: %v", err)
	}
	if !bytes.Equal(gotLegacy, legacyBinary) {
		t.Fatalf("migrated legacy binary mismatch: got %q", gotLegacy)
	}

	for _, tc := range []struct {
		name        string
		tag         string
		wantErr     string
		archiveMax  int
		expandedMax int
	}{
		{name: "wrong-version", tag: "v9.9.8", wantErr: "reports v9.9.9, expected v9.9.8"},
		{name: "wrong-checksum", tag: "v9.9.7", wantErr: "SHA256 mismatch"},
		{name: "archive-limit", tag: "v9.9.9", archiveMax: archive.Len() - 1, wantErr: "download exceeds"},
		{name: "expanded-limit", tag: "v9.9.9", expandedMax: len(binary) - 1, wantErr: "expanded binary exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failedBase := t.TempDir()
			failedInstallDir := filepath.Join(failedBase, "bin")
			cmd := exec.Command("bash", "install.sh")
			env := append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN", "METIS_MAX_ARCHIVE_BYTES", "METIS_MAX_EXPANDED_BYTES"),
				"METIS_GITHUB_WEB_BASE="+server.URL,
				"METIS_INSTALL_DIR="+failedInstallDir,
				"METIS_VERSION="+tc.tag,
				"METIS_REPO=Ricardo-M-L/metis",
			)
			if tc.archiveMax > 0 {
				env = append(env, fmt.Sprintf("METIS_MAX_ARCHIVE_BYTES=%d", tc.archiveMax))
			}
			if tc.expandedMax > 0 {
				env = append(env, fmt.Sprintf("METIS_MAX_EXPANDED_BYTES=%d", tc.expandedMax))
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("install unexpectedly succeeded:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("install error %q does not contain %q", out, tc.wantErr)
			}
			if _, err := os.Lstat(filepath.Join(failedInstallDir, "metis")); !os.IsNotExist(err) {
				t.Fatalf("failed install activated a launcher: %v", err)
			}
		})
	}
}

func TestInstallerRefusesToReplaceUnmanagedLauncher(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	baseDir := t.TempDir()
	installDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	launcher := filepath.Join(installDir, "metis")
	want := []byte("#!/bin/sh\necho custom-launcher\n")
	if err := os.WriteFile(launcher, want, 0o755); err != nil {
		t.Fatalf("write custom launcher: %v", err)
	}

	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
		"METIS_GITHUB_WEB_BASE=http://127.0.0.1:1",
		"METIS_INSTALL_DIR="+installDir,
		"METIS_VERSION=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install unexpectedly replaced custom launcher:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to replace unmanaged launcher") {
		t.Fatalf("unexpected refusal message: %s", out)
	}
	got, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatalf("read custom launcher after refusal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("custom launcher changed: got %q, want %q", got, want)
	}
}

func TestInstallerDoesNotStealLiveInstallLock(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	baseDir := t.TempDir()
	installDir := filepath.Join(baseDir, "bin")
	lockDir := filepath.Join(baseDir, "share", "metis", "locks", "install.lock.d")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("create install lock: %v", err)
	}
	owner := fmt.Sprintf(`{"pid":%d,"nonce":"owned-by-test","created_at":1}`+"\n", os.Getpid())
	ownerPath := filepath.Join(lockDir, "owner.json")
	if err := os.WriteFile(ownerPath, []byte(owner), 0o600); err != nil {
		t.Fatalf("write install lock owner: %v", err)
	}

	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
		"METIS_GITHUB_WEB_BASE=http://127.0.0.1:1",
		"METIS_INSTALL_DIR="+installDir,
		"METIS_VERSION=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install unexpectedly stole a live lock:\n%s", out)
	}
	if !strings.Contains(string(out), "already running") {
		t.Fatalf("unexpected lock error: %s", out)
	}
	got, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatalf("live owner file was removed: %v", err)
	}
	if string(got) != owner {
		t.Fatalf("live owner file changed: got %q, want %q", got, owner)
	}
}

func TestInstallerDoesNotRemoveLockWhenOwnerStateDisappears(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	requestSeen := make(chan struct{}, 1)
	continueRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestSeen <- struct{}{}
		<-continueRequest
		http.Error(w, "stop after lock mutation", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	baseDir := t.TempDir()
	installDir := filepath.Join(baseDir, "bin")
	lockDir := filepath.Join(baseDir, "share", "metis", "locks", "install.lock.d")
	ownerPath := filepath.Join(lockDir, "owner.json")
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
		"METIS_GITHUB_WEB_BASE="+server.URL,
		"METIS_INSTALL_DIR="+installDir,
		"METIS_VERSION=v1.0.0",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start installer: %v", err)
	}
	select {
	case <-requestSeen:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("installer did not reach asset request")
	}
	if err := os.Remove(ownerPath); err != nil {
		close(continueRequest)
		_ = cmd.Wait()
		t.Fatalf("remove owner state during install: %v", err)
	}
	close(continueRequest)
	if err := cmd.Wait(); err == nil {
		t.Fatalf("installer unexpectedly succeeded after test server failure:\n%s", output.Bytes())
	}
	info, err := os.Lstat(lockDir)
	if err != nil {
		t.Fatalf("installer removed lock directory after losing owner state: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("lock path changed after owner state disappeared: %v", info.Mode())
	}
}

func TestInstallerCrashLeavesOnlyCompletePendingLock(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	baseDir := t.TempDir()
	hookDir := newInstallerHook(t, "pending")
	cmd, output := startPausedInstaller(t, baseDir, hookDir)
	waitForPath(t, filepath.Join(hookDir, "pending.ready"))

	locksRoot := filepath.Join(baseDir, "share", "metis", "locks")
	fixed := filepath.Join(locksRoot, "install.lock.d")
	if _, err := os.Lstat(fixed); !os.IsNotExist(err) {
		t.Fatalf("pending claimant published fixed lock before resume: %v", err)
	}
	pending, err := filepath.Glob(filepath.Join(locksRoot, "install.lock.d.pending.*"))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending lock paths = %v, err=%v", pending, err)
	}
	owner := readTestInstallOwner(t, pending[0])
	if owner.PID != cmd.Process.Pid || owner.Nonce == "" {
		t.Fatalf("pending owner = %+v, process pid=%d", owner, cmd.Process.Pid)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill paused installer: %v", err)
	}
	_ = cmd.Wait()
	if _, err := os.Lstat(fixed); !os.IsNotExist(err) {
		t.Fatalf("crashed pending claimant left a fixed lock: %v\n%s", err, output.Bytes())
	}
	owner = readTestInstallOwner(t, pending[0])
	if owner.PID != cmd.Process.Pid {
		t.Fatalf("crashed pending owner changed: %+v", owner)
	}

	secondHook := newInstallerHook(t, "acquired")
	second, secondOutput := startPausedInstaller(t, baseDir, secondHook)
	waitForPath(t, filepath.Join(secondHook, "acquired.ready"))
	current := readTestInstallOwner(t, fixed)
	if current.PID != second.Process.Pid {
		t.Fatalf("second installer did not own fixed lock: %+v", current)
	}
	resumeInstallerHook(t, secondHook, "acquired")
	if err := waitInstaller(second, 5*time.Second); err == nil {
		t.Fatalf("second installer unexpectedly succeeded against closed test endpoint:\n%s", secondOutput.Bytes())
	}
	if _, err := os.Lstat(fixed); !os.IsNotExist(err) {
		t.Fatalf("second installer did not release fixed lock: %v", err)
	}
}

func TestInstallerConcurrentPublishersHaveOneFixedOwner(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	baseDir := t.TempDir()
	firstHook := newInstallerHook(t, "pending")
	first, firstOutput := startPausedInstaller(t, baseDir, firstHook)
	waitForPath(t, filepath.Join(firstHook, "pending.ready"))

	secondHook := newInstallerHook(t, "acquired")
	second, secondOutput := startPausedInstaller(t, baseDir, secondHook)
	waitForPath(t, filepath.Join(secondHook, "acquired.ready"))
	fixed := filepath.Join(baseDir, "share", "metis", "locks", "install.lock.d")
	secondOwner := readTestInstallOwner(t, fixed)
	if secondOwner.PID != second.Process.Pid {
		t.Fatalf("fixed owner = %+v, want second pid %d", secondOwner, second.Process.Pid)
	}

	resumeInstallerHook(t, firstHook, "pending")
	if err := waitInstaller(first, 5*time.Second); err == nil {
		t.Fatalf("losing publisher unexpectedly succeeded:\n%s", firstOutput.Bytes())
	}
	if !strings.Contains(firstOutput.String(), "already running") {
		t.Fatalf("losing publisher error = %q", firstOutput.String())
	}
	stillSecond := readTestInstallOwner(t, fixed)
	if stillSecond.PID != secondOwner.PID || stillSecond.Nonce != secondOwner.Nonce {
		t.Fatalf("losing publisher changed fixed owner: before=%+v after=%+v", secondOwner, stillSecond)
	}

	resumeInstallerHook(t, secondHook, "acquired")
	if err := waitInstaller(second, 5*time.Second); err == nil {
		t.Fatalf("winning installer unexpectedly succeeded against closed test endpoint:\n%s", secondOutput.Bytes())
	}
}

func TestInstallerRecoversACompleteGuardAfterReclaimerCrash(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	baseDir := t.TempDir()
	locksRoot := filepath.Join(baseDir, "share", "metis", "locks")
	target := testInstallOwner{PID: 99_999_991, Nonce: "dead-target", CreatedAt: 1}
	fixed := filepath.Join(locksRoot, "install.lock.d")
	writeTestInstallOwner(t, fixed, target)

	firstHook := newInstallerHook(t, "guard-acquired")
	first, firstOutput := startPausedInstaller(t, baseDir, firstHook)
	waitForPath(t, filepath.Join(firstHook, "guard-acquired.ready"))
	guard := filepath.Join(locksRoot, "install.reclaim."+target.Nonce+".d")
	firstGuard := readTestReclaimOwner(t, guard)
	if firstGuard.PID != first.Process.Pid || firstGuard.TargetPID != target.PID || firstGuard.TargetNonce != target.Nonce {
		t.Fatalf("first reclaim guard = %+v, process pid=%d target=%+v", firstGuard, first.Process.Pid, target)
	}

	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill first reclaimer: %v", err)
	}
	_ = first.Wait()
	if got := readTestInstallOwner(t, fixed); got != target {
		t.Fatalf("crashed reclaimer changed target lock: got %+v, want %+v\n%s", got, target, firstOutput.Bytes())
	}
	if got := readTestReclaimOwner(t, guard); got != firstGuard {
		t.Fatalf("crashed reclaimer left an incomplete guard: got %+v, want %+v", got, firstGuard)
	}

	secondHook := newInstallerHook(t, "acquired")
	second, secondOutput := startPausedInstaller(t, baseDir, secondHook)
	waitForPath(t, filepath.Join(secondHook, "acquired.ready"))
	if got := readTestInstallOwner(t, fixed); got.PID != second.Process.Pid {
		t.Fatalf("second reclaimer did not acquire fixed lock: %+v", got)
	}
	retired := guard + ".retired." + firstGuard.Nonce
	if got := readTestReclaimOwner(t, retired); got != firstGuard {
		t.Fatalf("retired guard = %+v, want %+v", got, firstGuard)
	}

	resumeInstallerHook(t, secondHook, "acquired")
	if err := waitInstaller(second, 5*time.Second); err == nil {
		t.Fatalf("second reclaimer unexpectedly completed install:\n%s", secondOutput.Bytes())
	}
	if got := readTestReclaimOwner(t, retired); got != firstGuard {
		t.Fatalf("retired ABA tombstone changed after guard release: got %+v, want %+v", got, firstGuard)
	}
}

func TestInstallerPausedGuardObserverCannotMoveSuccessor(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	baseDir := t.TempDir()
	locksRoot := filepath.Join(baseDir, "share", "metis", "locks")
	target := testInstallOwner{PID: 99_999_992, Nonce: "aba-target", CreatedAt: 1}
	fixed := filepath.Join(locksRoot, "install.lock.d")
	writeTestInstallOwner(t, fixed, target)
	guardPath := filepath.Join(locksRoot, "install.reclaim."+target.Nonce+".d")
	staleGuard := testReclaimOwner{
		PID:         99_999_993,
		Nonce:       "stale-guard",
		TargetPID:   target.PID,
		TargetNonce: target.Nonce,
		CreatedAt:   1,
	}
	writeTestReclaimOwner(t, guardPath, staleGuard)

	firstHook := newInstallerHook(t, "guard-dead-observed")
	first, firstOutput := startPausedInstaller(t, baseDir, firstHook)
	waitForPath(t, filepath.Join(firstHook, "guard-dead-observed.ready"))

	secondHook := newInstallerHook(t, "acquired")
	second, secondOutput := startPausedInstaller(t, baseDir, secondHook)
	waitForPath(t, filepath.Join(secondHook, "acquired.ready"))
	secondOwner := readTestInstallOwner(t, fixed)
	if secondOwner.PID != second.Process.Pid {
		t.Fatalf("successor fixed owner = %+v, want pid %d", secondOwner, second.Process.Pid)
	}

	resumeInstallerHook(t, firstHook, "guard-dead-observed")
	if err := waitInstaller(first, 5*time.Second); err == nil {
		t.Fatalf("paused stale observer unexpectedly succeeded:\n%s", firstOutput.Bytes())
	}
	if got := readTestInstallOwner(t, fixed); got != secondOwner {
		t.Fatalf("stale observer moved successor: before=%+v after=%+v", secondOwner, got)
	}
	retired := guardPath + ".retired." + staleGuard.Nonce
	if got := readTestReclaimOwner(t, retired); got != staleGuard {
		t.Fatalf("retired stale guard = %+v, want %+v", got, staleGuard)
	}

	resumeInstallerHook(t, secondHook, "acquired")
	if err := waitInstaller(second, 5*time.Second); err == nil {
		t.Fatalf("successor installer unexpectedly completed install:\n%s", secondOutput.Bytes())
	}
}

func TestInstallerRejectsManagedRootSymlinksWithoutFollowingThem(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	tests := []struct {
		name        string
		linkPath    func(base string) string
		regularFile bool
	}{
		{name: "managed-root", linkPath: func(base string) string {
			return filepath.Join(base, "share", "metis")
		}},
		{name: "versions-root", linkPath: func(base string) string {
			return filepath.Join(base, "share", "metis", "versions")
		}},
		{name: "versions-non-directory", regularFile: true, linkPath: func(base string) string {
			return filepath.Join(base, "share", "metis", "versions")
		}},
		{name: "staging-root", linkPath: func(base string) string {
			return filepath.Join(base, "share", "metis", "staging")
		}},
		{name: "locks-root", linkPath: func(base string) string {
			return filepath.Join(base, "share", "metis", "locks")
		}},
		{name: "running-root", linkPath: func(base string) string {
			return filepath.Join(base, "share", "metis", "locks", "running")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			installDir := filepath.Join(baseDir, "bin")
			external := filepath.Join(baseDir, "external")
			if err := os.MkdirAll(external, 0o755); err != nil {
				t.Fatalf("create external sentinel directory: %v", err)
			}
			markerPath := filepath.Join(external, "marker")
			marker := []byte("must-not-change")
			if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
				t.Fatalf("write external sentinel: %v", err)
			}

			linkPath := tc.linkPath(baseDir)
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				t.Fatalf("create managed parent: %v", err)
			}
			if tc.regularFile {
				if err := os.WriteFile(linkPath, []byte("not-a-directory"), 0o600); err != nil {
					t.Fatalf("create managed non-directory: %v", err)
				}
			} else if err := os.Symlink(external, linkPath); err != nil {
				t.Fatalf("create managed-root symlink: %v", err)
			}

			cmd := exec.Command("bash", "install.sh")
			cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
				"METIS_GITHUB_WEB_BASE=http://127.0.0.1:1",
				"METIS_INSTALL_DIR="+installDir,
				"METIS_VERSION=v1.0.0",
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("installer unexpectedly followed %s:\n%s", tc.name, out)
			}
			if !strings.Contains(string(out), "refusing to use symlink or non-directory as managed directory") {
				t.Fatalf("unexpected managed-root safety error: %s", out)
			}
			got, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatalf("external sentinel was removed: %v", err)
			}
			if !bytes.Equal(got, marker) {
				t.Fatalf("external sentinel changed: got %q, want %q", got, marker)
			}
			entries, err := os.ReadDir(external)
			if err != nil {
				t.Fatalf("read external sentinel directory: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != "marker" {
				t.Fatalf("installer created content through symlink: %v", entries)
			}
			info, err := os.Lstat(linkPath)
			if err != nil {
				t.Fatalf("managed unsafe path was replaced: %v", err)
			}
			if tc.regularFile && !info.Mode().IsRegular() {
				t.Fatalf("managed non-directory changed type: %v", info.Mode())
			}
			if !tc.regularFile && info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("managed-root symlink was replaced: %v", info.Mode())
			}
		})
	}
}

func TestInstallerRetainsCurrentAndTwoPreviousVersions(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer only supports macOS and Linux")
	}

	target := runtime.GOOS + "-" + runtime.GOARCH
	artifact := "metis-" + target + ".tar.gz"
	type releaseFiles struct {
		archive []byte
		sum     string
	}
	releases := make(map[string]releaseFiles)
	for _, version := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0+build.1"} {
		binary := []byte(fmt.Sprintf("#!/bin/sh\necho 'v%s (Metis)'\n", version))
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "metis-" + target, Mode: 0o755, Size: int64(len(binary))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(binary); err != nil {
			t.Fatalf("write binary: %v", err)
		}
		if err := tw.Close(); err != nil {
			t.Fatalf("close tar: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("close gzip: %v", err)
		}
		sum := sha256.Sum256(archive.Bytes())
		releases["v"+version] = releaseFiles{
			archive: append([]byte(nil), archive.Bytes()...),
			sum:     fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), artifact),
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/Ricardo-M-L/metis/releases/download/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		tag, name, ok := strings.Cut(rest, "/")
		files, exists := releases[tag]
		if !ok || !exists {
			http.NotFound(w, r)
			return
		}
		switch name {
		case artifact:
			_, _ = w.Write(files.archive)
		case artifact + ".sha256":
			_, _ = w.Write([]byte(files.sum))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	baseDir := t.TempDir()
	installDir := filepath.Join(baseDir, "bin")
	stagingRoot := filepath.Join(baseDir, "share", "metis", "staging")
	staleStage := filepath.Join(stagingRoot, ".install.stale")
	if err := os.MkdirAll(staleStage, 0o755); err != nil {
		t.Fatalf("create stale staging directory: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleStage, old, old); err != nil {
		t.Fatalf("age stale staging directory: %v", err)
	}
	sentinel := filepath.Join(baseDir, "sentinel")
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatalf("create staging symlink sentinel: %v", err)
	}
	stagingSymlink := filepath.Join(stagingRoot, ".install.external")
	if err := os.Symlink(sentinel, stagingSymlink); err != nil {
		t.Fatalf("create staging symlink: %v", err)
	}
	runInstall := func(tag string) {
		t.Helper()
		cmd := exec.Command("bash", "install.sh")
		cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
			"METIS_GITHUB_WEB_BASE="+server.URL,
			"METIS_INSTALL_DIR="+installDir,
			"METIS_VERSION="+tag,
			"METIS_REPO=Ricardo-M-L/metis",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install %s failed: %v\n%s", tag, err, out)
		}
	}
	for _, tag := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		runInstall(tag)
	}
	if _, err := os.Stat(staleStage); !os.IsNotExist(err) {
		t.Fatalf("stale staging directory was not removed: %v", err)
	}
	if info, err := os.Lstat(stagingSymlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("staging cleanup followed or removed an external symlink: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("staging cleanup damaged symlink target: %v", err)
	}

	versionsDir := filepath.Join(baseDir, "share", "metis", "versions")
	runningLockDir := filepath.Join(baseDir, "share", "metis", "locks", "running", "1.0.0")
	if err := os.MkdirAll(runningLockDir, 0o755); err != nil {
		t.Fatalf("create running lock directory: %v", err)
	}
	pid := os.Getpid()
	lock := fmt.Sprintf(`{"pid":%d,"nonce":"test","version":"1.0.0","exec_path":"test","created_at":1}`+"\n", pid)
	if err := os.WriteFile(filepath.Join(runningLockDir, fmt.Sprintf("%d.json", pid)), []byte(lock), 0o600); err != nil {
		t.Fatalf("write running lock: %v", err)
	}

	runInstall("v1.3.0+build.1")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("read versions directory: %v", err)
	}
	var got []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			got = append(got, entry.Name())
		}
	}
	want := []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0+build.1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("versions with a live running lock = %v, want %v", got, want)
	}

	runningRoot := filepath.Join(baseDir, "share", "metis", "locks", "running")
	if err := os.RemoveAll(runningRoot); err != nil {
		t.Fatalf("remove running lock: %v", err)
	}
	unknownVersionDir := filepath.Join(runningRoot, "1.0.0")
	if err := os.MkdirAll(unknownVersionDir, 0o755); err != nil {
		t.Fatalf("create unknown running-lock directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unknownVersionDir, "unexpected.entry"), []byte("unknown\n"), 0o600); err != nil {
		t.Fatalf("write unknown running-lock entry: %v", err)
	}
	runInstall("v1.3.0+build.1")
	entries, err = os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("read versions protected by unknown running-lock entry: %v", err)
	}
	got = got[:0]
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			got = append(got, entry.Name())
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unknown running-lock entry did not protect all versions: got %v, want %v", got, want)
	}
	if err := os.RemoveAll(runningRoot); err != nil {
		t.Fatalf("remove unknown running-lock entry: %v", err)
	}
	if err := os.Symlink(sentinel, runningRoot); err != nil {
		t.Fatalf("create unknown running-lock symlink: %v", err)
	}
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(withoutEnv(os.Environ(), "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"),
		"METIS_GITHUB_WEB_BASE="+server.URL,
		"METIS_INSTALL_DIR="+installDir,
		"METIS_VERSION=v1.3.0+build.1",
		"METIS_REPO=Ricardo-M-L/metis",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install unexpectedly accepted a running-root symlink:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to use symlink or non-directory as managed directory") {
		t.Fatalf("unexpected running-root safety error: %s", out)
	}
	entries, err = os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("read versions protected by unknown lock state: %v", err)
	}
	got = got[:0]
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			got = append(got, entry.Name())
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unknown running-lock state did not protect all versions: got %v, want %v", got, want)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("running-lock inspection followed or damaged symlink target: %v", err)
	}
	if err := os.Remove(runningRoot); err != nil {
		t.Fatalf("remove unknown running-lock symlink: %v", err)
	}

	runInstall("v1.3.0+build.1")
	entries, err = os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("read pruned versions directory: %v", err)
	}
	got = got[:0]
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			got = append(got, entry.Name())
		}
	}
	want = []string{"1.1.0", "1.2.0", "1.3.0+build.1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("retained versions after lock release = %v, want %v", got, want)
	}

	launcher := filepath.Join(installDir, "metis")
	resolved, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		t.Fatalf("resolve launcher: %v", err)
	}
	wantCurrent := filepath.Join(versionsDir, "1.3.0+build.1", "metis")
	wantCurrent, err = filepath.EvalSymlinks(wantCurrent)
	if err != nil {
		t.Fatalf("resolve expected current binary: %v", err)
	}
	if resolved != wantCurrent {
		t.Fatalf("launcher target = %q, want %q", resolved, wantCurrent)
	}
	if out, err := exec.Command(launcher, "version").CombinedOutput(); err != nil || !strings.Contains(string(out), "v1.3.0+build.1") {
		t.Fatalf("launcher did not run current version: err=%v output=%q", err, out)
	}
}

func withoutEnv(env []string, names ...string) []string {
	blocked := make(map[string]bool, len(names))
	for _, name := range names {
		blocked[name] = true
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type testInstallOwner struct {
	PID       int    `json:"pid"`
	Nonce     string `json:"nonce"`
	CreatedAt int64  `json:"created_at"`
}

type testReclaimOwner struct {
	PID         int    `json:"pid"`
	Nonce       string `json:"nonce"`
	TargetPID   int    `json:"target_pid"`
	TargetNonce string `json:"target_nonce"`
	CreatedAt   int64  `json:"created_at"`
}

func newInstallerHook(t *testing.T, phase string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, phase+".pause"), []byte("pause\n"), 0o600); err != nil {
		t.Fatalf("create %s installer hook: %v", phase, err)
	}
	return dir
}

func startPausedInstaller(t *testing.T, baseDir, hookDir string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(withoutEnv(os.Environ(),
		"METIS_GITHUB_TOKEN", "GITHUB_TOKEN", "METIS_INSTALLER_TEST_HOOK_DIR"),
		"METIS_GITHUB_WEB_BASE=http://127.0.0.1:1",
		"METIS_INSTALL_DIR="+filepath.Join(baseDir, "bin"),
		"METIS_VERSION=v1.0.0",
		"METIS_INSTALLER_TEST_HOOK_DIR="+hookDir,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start paused installer: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, &output
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect awaited path %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func resumeInstallerHook(t *testing.T, hookDir, phase string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(hookDir, phase+".continue"), []byte("continue\n"), 0o600); err != nil {
		t.Fatalf("resume %s installer hook: %v", phase, err)
	}
}

func waitInstaller(cmd *exec.Cmd, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("installer did not exit within %s", timeout)
	}
}

func readTestInstallOwner(t *testing.T, lockDir string) testInstallOwner {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(lockDir, "owner.json"))
	if err != nil {
		t.Fatalf("read install lock owner from %s: %v", lockDir, err)
	}
	var owner testInstallOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatalf("decode install lock owner from %s: %v", lockDir, err)
	}
	if owner.PID <= 0 || owner.Nonce == "" || owner.CreatedAt <= 0 {
		t.Fatalf("invalid install lock owner from %s: %+v", lockDir, owner)
	}
	return owner
}

func writeTestInstallOwner(t *testing.T, lockDir string, owner testInstallOwner) {
	t.Helper()
	writeTestJSONOwner(t, lockDir, owner)
}

func readTestReclaimOwner(t *testing.T, lockDir string) testReclaimOwner {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(lockDir, "owner.json"))
	if err != nil {
		t.Fatalf("read reclaim guard owner from %s: %v", lockDir, err)
	}
	var owner testReclaimOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatalf("decode reclaim guard owner from %s: %v", lockDir, err)
	}
	if owner.PID <= 0 || owner.Nonce == "" || owner.TargetPID <= 0 || owner.TargetNonce == "" || owner.CreatedAt <= 0 {
		t.Fatalf("invalid reclaim guard owner from %s: %+v", lockDir, owner)
	}
	return owner
}

func writeTestReclaimOwner(t *testing.T, lockDir string, owner testReclaimOwner) {
	t.Helper()
	writeTestJSONOwner(t, lockDir, owner)
}

func writeTestJSONOwner(t *testing.T, lockDir string, owner any) {
	t.Helper()
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("create test lock directory %s: %v", lockDir, err)
	}
	data, err := json.Marshal(owner)
	if err != nil {
		t.Fatalf("encode test lock owner: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(lockDir, "owner.json"), data, 0o600); err != nil {
		t.Fatalf("write test lock owner to %s: %v", lockDir, err)
	}
}
