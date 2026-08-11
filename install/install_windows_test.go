//go:build windows

package install_test

import (
	"archive/zip"
	"bytes"
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

func TestPowerShellInstallerSupportsAnonymousPublicRelease(t *testing.T) {
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skip("unsupported Windows test architecture")
	}
	artifact := "metis-windows-" + arch + ".zip"
	binaryName := "metis.exe"
	binary := []byte("test-windows-binary")

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create(binaryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	sumFile := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), artifact)
	serveBadChecksum := false

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
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/v9.9.9/"+artifact, func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(r)
		_, _ = w.Write(archive.Bytes())
	})
	mux.HandleFunc("/Ricardo-M-L/metis/releases/download/v9.9.9/"+artifact+".sha256", func(w http.ResponseWriter, r *http.Request) {
		assertAnonymous(r)
		if serveBadChecksum {
			_, _ = fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), artifact)
			return
		}
		_, _ = w.Write([]byte(sumFile))
	})

	installDir := t.TempDir()
	runInstaller := func() ([]byte, error) {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1",
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
		out, err := runInstaller()
		if err != nil {
			t.Fatalf("anonymous PowerShell install attempt %d failed: %v\n%s", attempt, err, out)
		}
	}
	got, err := os.ReadFile(filepath.Join(installDir, "metis.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed binary mismatch: got %q", got)
	}

	sentinel := []byte("keep-existing-install")
	if err := os.WriteFile(filepath.Join(installDir, "metis.exe"), sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	serveBadChecksum = true
	if out, err := runInstaller(); err == nil {
		t.Fatalf("installer accepted a bad checksum:\n%s", out)
	}
	got, err = os.ReadFile(filepath.Join(installDir, "metis.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("failed install changed existing binary: got %q", got)
	}
}
