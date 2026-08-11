package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeExecutable(version, marker string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'v" + version + " (Metis)'; exit 0; fi\necho '" + marker + "'\n")
}

// buildFakeRelease spins up an httptest server that mimics the GitHub
// release-assets API surface Apply touches: /latest returns metadata with
// two assets (tarball + sha256), /assets/:id serves the bytes. Returns the
// server and a *release pointing at it.
func buildFakeRelease(t *testing.T, version, target string, binContent []byte) (*httptest.Server, *release) {
	t.Helper()

	// Build the tarball in memory: metis-<target> at the root.
	var tarBuf strings.Builder
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	inner := fmt.Sprintf("metis-%s", target)
	if err := tw.WriteHeader(&tar.Header{
		Name:     inner,
		Mode:     0o755,
		Size:     int64(len(binContent)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(binContent); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	tarball := []byte(tarBuf.String())

	sum := sha256.Sum256(tarball)
	sumLine := fmt.Sprintf("%s  metis-%s.tar.gz\n", hex.EncodeToString(sum[:]), target)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &release{
		TagName: "v" + version,
		Assets: []asset{
			{ID: 1, Name: fmt.Sprintf("metis-%s.tar.gz", target)},
			{ID: 2, Name: fmt.Sprintf("metis-%s.tar.gz.sha256", target)},
		},
	}

	repo := Repo()
	assertNoEmptyBearer := func(r *http.Request) {
		t.Helper()
		if got := r.Header.Get("Authorization"); got == "Bearer " {
			t.Errorf("request sent an empty bearer token")
		}
	}
	mux.HandleFunc("/repos/"+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assertNoEmptyBearer(r)
		json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/repos/"+repo+"/releases/assets/1", func(w http.ResponseWriter, r *http.Request) {
		assertNoEmptyBearer(r)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(tarball)
	})
	mux.HandleFunc("/repos/"+repo+"/releases/assets/2", func(w http.ResponseWriter, r *http.Request) {
		assertNoEmptyBearer(r)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(sumLine))
	})
	return srv, rel
}

// TestApply_VersionedInstall pins the claude-code-style layout:
// binary lands at <base>/share/metis/versions/<ver>/metis and
// <base>/bin/metis is a symlink pointing at it. Uses a fake release
// server so no network or token is needed.
func TestApply_VersionedInstall(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix symlink layout test")
	}
	// Redirect the API base Apply hits to our fake server.
	srv, rel := buildFakeRelease(t, "0.5.0", "test-os-arch", fakeExecutable("0.5.0", "fake-metis-0.5.0"))
	oldAPI := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = oldAPI })
	oldTarget := targetForTest
	targetForTest = "test-os-arch"
	t.Cleanup(func() { targetForTest = oldTarget })

	// Fake "user install": <tmp>/bin/metis is the symlink Apply manages.
	base := t.TempDir()
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	destLink := filepath.Join(binDir, "metis")
	// Pre-existing symlink at an older version, so Apply must REPLACE it.
	oldVersionDir := filepath.Join(base, "share", "metis", "versions", "0.4.0")
	if err := os.MkdirAll(oldVersionDir, 0o755); err != nil {
		t.Fatalf("mkdir old version: %v", err)
	}
	oldBin := filepath.Join(oldVersionDir, "metis")
	if err := os.WriteFile(oldBin, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old bin: %v", err)
	}
	if err := os.Symlink(oldBin, destLink); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	if err := Apply(context.Background(), "", destLink, rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. New version binary exists at the versioned path.
	newBin := filepath.Join(base, "share", "metis", "versions", "0.5.0", "metis")
	if _, err := os.Stat(newBin); err != nil {
		t.Fatalf("versioned binary missing: %v", err)
	}
	// 2. Symlink points at the new version.
	target, err := os.Readlink(destLink)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != newBin {
		t.Errorf("symlink → %q, want %q", target, newBin)
	}
	// 3. Old version directory is preserved (rollback capability).
	if _, err := os.Stat(oldBin); err != nil {
		t.Errorf("old version should be preserved: %v", err)
	}
	// 4. Resolved binary is executable.
	info, err := os.Stat(newBin)
	if err == nil && info.Mode()&0o111 == 0 {
		t.Errorf("versioned binary not executable: mode=%v", info.Mode())
	}
}

// TestApply_SymlinkSwapIsAtomic verifies the destPath swap uses rename(2)
// semantics: a reader resolving destPath mid-Apply sees either the old or
// the new link, never a missing file. We can't truly race this in a unit
// test, but we can assert the post-condition that destPath is always a
// valid symlink after Apply returns.
func TestApply_SymlinkSwapIsAtomic(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix symlink layout test")
	}
	srv, rel := buildFakeRelease(t, "0.5.1", "test-os-arch", fakeExecutable("0.5.1", "fake"))
	oldAPI := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = oldAPI })
	oldTarget := targetForTest
	targetForTest = "test-os-arch"
	t.Cleanup(func() { targetForTest = oldTarget })

	base := t.TempDir()
	binDir := filepath.Join(base, "bin")
	os.MkdirAll(binDir, 0o755)
	destLink := filepath.Join(binDir, "metis")

	if err := Apply(context.Background(), "fake-token", destLink, rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// destPath must resolve to a real file immediately after Apply.
	if _, err := os.Stat(destLink); err != nil {
		t.Fatalf("destPath does not resolve after Apply: %v", err)
	}
}

func TestApplyRetainsCurrentPlusTwoOldVersions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix Apply layout test")
	}
	oldTarget := targetForTest
	targetForTest = "test-os-arch"
	t.Cleanup(func() { targetForTest = oldTarget })
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 5; i++ {
		version := fmt.Sprintf("1.0.%d", i)
		srv, rel := buildFakeRelease(t, version, "test-os-arch", fakeExecutable(version, version))
		oldAPI := apiBase
		apiBase = srv.URL
		if err := Apply(context.Background(), "", launcher, rel); err != nil {
			apiBase = oldAPI
			t.Fatalf("Apply %s: %v", version, err)
		}
		apiBase = oldAPI
		// Make ordering deterministic even on filesystems with coarse mtimes.
		bin := filepath.Join(base, "share", "metis", "versions", version, "metis")
		when := time.Unix(int64(i), 0)
		if err := os.Chtimes(bin, when, when); err != nil {
			t.Fatal(err)
		}
	}
	// The final Apply pruned using pre-adjustment mtimes; run normal no-release
	// housekeeping once to assert the stable retention invariant.
	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(base, "share", "metis", "versions"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		if entry.IsDir() {
			got = append(got, entry.Name())
		}
	}
	if strings.Join(got, ",") != "1.0.3,1.0.4,1.0.5" {
		t.Fatalf("retained versions = %v, want current plus two old", got)
	}
}

func TestApplyAnonymousDownloadUsesBrowserURL(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix fake executable fixture")
	}
	archive := makeTarArchive(t, "metis-test-os-arch", fakeExecutable("2.0.0", "direct"), tar.TypeReg)
	sum := sha256.Sum256(archive)
	var assetAPICalls int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/download/archive", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous direct download sent Authorization %q", got)
		}
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/download/checksum", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  metis-test-os-arch.tar.gz\n", hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		assetAPICalls++
		http.Error(w, "asset API must not be used", http.StatusTooManyRequests)
	})
	rel := &release{TagName: "v2.0.0", Assets: []asset{
		{ID: 1, Name: "metis-test-os-arch.tar.gz", BrowserDownloadURL: srv.URL + "/download/archive", Size: int64(len(archive))},
		{ID: 2, Name: "metis-test-os-arch.tar.gz.sha256", BrowserDownloadURL: srv.URL + "/download/checksum"},
	}}
	oldAPI, oldTarget := apiBase, targetForTest
	apiBase, targetForTest = srv.URL, "test-os-arch"
	t.Cleanup(func() { apiBase, targetForTest = oldAPI, oldTarget })
	launcher := filepath.Join(t.TempDir(), "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), "", launcher, rel); err != nil {
		t.Fatalf("Apply anonymous direct release: %v", err)
	}
	if assetAPICalls != 0 {
		t.Fatalf("anonymous download made %d asset API calls", assetAPICalls)
	}
}

func TestAnonymousLatestAndApplyAvoidGitHubAssetAPI(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix fake executable fixture")
	}
	oldTarget := targetForTest
	targetForTest = "test-os-arch"
	t.Cleanup(func() { targetForTest = oldTarget })
	archiveName := "metis-test-os-arch.tar.gz"
	archive := makeTarArchive(t, "metis-test-os-arch", fakeExecutable("3.0.0", "web-fallback"), tar.TypeReg)
	sum := sha256.Sum256(archive)
	var apiCalls int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/api/repos/", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		http.Error(w, "shared IP rate limited", http.StatusForbidden)
	})
	mux.HandleFunc("/"+Repo()+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous latest sent Authorization %q", got)
		}
		http.Redirect(w, r, "/"+Repo()+"/releases/tag/v3.0.0", http.StatusFound)
	})
	mux.HandleFunc("/"+Repo()+"/releases/tag/v3.0.0", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/"+Repo()+"/releases/download/v3.0.0/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous asset sent Authorization %q", got)
		}
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/"+Repo()+"/releases/download/v3.0.0/"+archiveName+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), archiveName)
	})
	oldAPI, oldWeb := apiBase, webBase
	apiBase, webBase = srv.URL+"/api", srv.URL
	t.Cleanup(func() { apiBase, webBase = oldAPI, oldWeb })

	rel, err := Latest(context.Background(), "")
	if err != nil {
		t.Fatalf("Latest anonymous web redirect: %v", err)
	}
	launcher := filepath.Join(t.TempDir(), "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), "", launcher, rel); err != nil {
		t.Fatalf("Apply anonymous deterministic assets: %v", err)
	}
	if apiCalls != 0 {
		t.Fatalf("anonymous flow made %d REST API calls", apiCalls)
	}
}

func TestVerifyCandidateRejectsWrongVersion(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix shell fixture")
	}
	path := filepath.Join(t.TempDir(), "metis")
	if err := os.WriteFile(path, fakeExecutable("1.0.0", "wrong"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyCandidate(context.Background(), path, "2.0.0"); err == nil {
		t.Fatal("verifyCandidate accepted binary reporting a different version")
	}
	if err := verifyCandidate(context.Background(), path, "1.0.0"); err != nil {
		t.Fatalf("verifyCandidate rejected matching version: %v", err)
	}
}

func TestVerifyCandidateRequiresVersionAsFirstField(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix shell fixture")
	}
	path := filepath.Join(t.TempDir(), "metis")
	content := []byte("#!/bin/sh\necho 'warning 1.2.3'\n")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyCandidate(context.Background(), path, "1.2.3"); err == nil {
		t.Fatal("verifyCandidate accepted expected version from a non-leading field")
	}
}

func TestApplyMigratesLegacyFlatLauncherAndKeepsRollback(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix flat launcher fixture")
	}
	oldTarget := targetForTest
	targetForTest = "test-os-arch"
	t.Cleanup(func() { targetForTest = oldTarget })
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := fakeExecutable("0.9.0", "legacy-flat")
	if err := os.WriteFile(launcher, legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, rel := buildFakeRelease(t, "1.0.0", "test-os-arch", fakeExecutable("1.0.0", "new"))
	oldAPI := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = oldAPI })

	if err := Apply(context.Background(), "", launcher, rel); err != nil {
		t.Fatalf("Apply legacy migration: %v", err)
	}
	rollback := filepath.Join(base, "share", "metis", "versions", "0.9.0", "metis")
	got, err := os.ReadFile(rollback)
	if err != nil {
		t.Fatalf("legacy rollback version missing: %v", err)
	}
	if !bytes.Equal(got, legacy) {
		t.Fatalf("legacy rollback bytes changed")
	}
	target, err := os.Readlink(launcher)
	if err != nil {
		t.Fatalf("launcher was not migrated to managed symlink: %v", err)
	}
	if !strings.Contains(target, filepath.Join("versions", "1.0.0", "metis")) {
		t.Fatalf("launcher points to %q, want new managed version", target)
	}
}

func TestApplyIfNeededSkipsCurrentButApplyStillForcesReinstall(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix fake executable fixture")
	}
	oldTarget := targetForTest
	targetForTest = "test-os-arch"
	t.Cleanup(func() { targetForTest = oldTarget })
	srv, rel := buildFakeRelease(t, "4.0.0", "test-os-arch", fakeExecutable("4.0.0", "force"))
	oldAPI := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = oldAPI })
	launcher := filepath.Join(t.TempDir(), "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), "", launcher, rel); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	// Closing the only asset source proves the automatic variant performs no
	// download for current, while the force-compatible Apply still attempts it.
	srv.Close()
	installed, err := ApplyIfNeeded(context.Background(), "", launcher, rel)
	if err != nil || installed {
		t.Fatalf("ApplyIfNeeded current = installed %v, err %v; want false,nil", installed, err)
	}
	if err := Apply(context.Background(), "", launcher, rel); err == nil {
		t.Fatal("Apply did not attempt force-compatible reinstall of current release")
	}
}

func TestExtractBinaryRejectsNestedBasenameAndNonRegularEntry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    string
		typeflag byte
	}{
		{name: "nested basename", entry: "nested/metis-test", typeflag: tar.TypeReg},
		{name: "symlink", entry: "metis-test", typeflag: tar.TypeSymlink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := makeTarArchive(t, tc.entry, []byte("payload"), tc.typeflag)
			path := filepath.Join(t.TempDir(), "archive.tar.gz")
			if err := os.WriteFile(path, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := extractBinary(path, t.TempDir(), "metis-test"); err == nil {
				t.Fatalf("extractBinary accepted %s", tc.name)
			}
		})
	}
}

func makeTarArchive(t *testing.T, entry string, content []byte, typeflag byte) []byte {
	t.Helper()
	var out strings.Builder
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	h := &tar.Header{Name: entry, Mode: 0o755, Size: int64(len(content)), Typeflag: typeflag}
	if typeflag == tar.TypeSymlink {
		h.Size = 0
		h.Linkname = "outside"
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if h.Size > 0 {
		if _, err := io.Copy(tw, strings.NewReader(string(content))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(out.String())
}
