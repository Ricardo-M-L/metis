package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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

	installDir := t.TempDir()
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
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed binary mismatch: got %q", got)
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
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
