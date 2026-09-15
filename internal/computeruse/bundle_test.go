package computeruse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type noBundleNetwork struct{ calls int }

func (n *noBundleNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls++
	return nil, errors.New("network forbidden by offline acceptance fixture")
}

func bundleFixture(t *testing.T) (*Manager, string, *noBundleNetwork) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("official component targets macOS")
	}
	binary := descriptionScript(t, fixtureDescription("1.2.3"))
	archive := archiveBytes(t, []tarEntry{{name: "bundle/bin/metis-cu", data: binary, mode: 0755}})
	m, _ := testRelease(t, archive)
	digest := sha256.Sum256(binary)
	m.manifest.Releases[0].BinarySHA256 = hex.EncodeToString(digest[:])
	bundle := t.TempDir()
	t.Setenv("METIS_CU_BUNDLE_DIR", bundle)
	archivePath := filepath.Join(bundle, "metis-cu.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0600); err != nil {
		t.Fatal(err)
	}
	network := &noBundleNetwork{}
	m.client = &http.Client{Transport: network}
	return m, archivePath, network
}

func TestEnsureAdoptsPinnedDesktopBundleOffline(t *testing.T) {
	m, _, network := bundleFixture(t)
	before, err := m.Status(context.Background())
	if err != nil || before.Installed || network.calls != 0 {
		t.Fatalf("status caused installation: %+v %v", before, err)
	}
	got, err := m.Ensure(context.Background())
	if err != nil || !got.Installed || got.Source != "official" || network.calls != 0 {
		t.Fatalf("offline bundled install: %+v, %v; requests=%d", got, err, network.calls)
	}
}

func TestEnsureRejectsTamperedBundleWithoutNetworkFallback(t *testing.T) {
	m, path, network := bundleFixture(t)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := m.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SHA256") || network.calls != 0 {
		t.Fatalf("corrupt bundle not rejected before network/probe: %v, requests=%d", err, network.calls)
	}
}

func TestEnsureRejectsSymlinkBundle(t *testing.T) {
	m, path, network := bundleFixture(t)
	moved := path + ".actual"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
	_, err := m.Ensure(context.Background())
	if err == nil || network.calls != 0 {
		t.Fatalf("symlink bundle accepted/fetched: %v", err)
	}
}
