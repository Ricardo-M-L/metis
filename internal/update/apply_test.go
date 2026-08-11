package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	// Redirect the API base Apply hits to our fake server.
	srv, rel := buildFakeRelease(t, "0.5.0", "test-os-arch", []byte("#!/bin/sh\necho fake-metis-0.5.0\n"))
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
	srv, rel := buildFakeRelease(t, "0.5.1", "test-os-arch", []byte("fake"))
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
