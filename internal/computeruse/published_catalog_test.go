package computeruse

import (
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Source builds ship the same reviewed, published helper pins as Desktop.
// An empty embedded catalog would make an already installed official helper
// unusable after a local CLI or Desktop rebuild.
func TestPublishedCatalogCoversSupportedDesktopTargets(t *testing.T) {
	manifest := PinnedManifest()
	if len(manifest.Releases) == 0 {
		t.Fatal("source builds must retain a published Computer Use catalog")
	}
	targets := map[string]int{"darwin-arm64": 0, "darwin-amd64": 0}
	for _, release := range manifest.Releases {
		if err := validateRelease(release); err != nil {
			t.Fatalf("invalid published pin for %s: %v", release.Target, err)
		}
		if release.ProtocolVersion != ProtocolVersion {
			continue
		}
		if _, supported := targets[release.Target]; !supported {
			continue
		}
		targets[release.Target]++
		if release.Format != "tar.gz" || release.BinaryPath != "metis-cu" || release.Size <= 0 {
			t.Fatalf("published Desktop artifact has incompatible archive layout: %+v", release)
		}
		location, err := url.Parse(release.URL)
		if err != nil || location.Host != "github.com" ||
			!strings.HasPrefix(location.Path, "/Ricardo-M-L/metis/releases/download/v") ||
			!strings.HasSuffix(location.Path, "/metis-cu-"+release.Target+".tar.gz") {
			t.Fatalf("published Desktop artifact lacks an explicit official release URL: %q", release.URL)
		}
	}
	for target, count := range targets {
		if count != 1 {
			t.Errorf("published catalog has %d compatible pins for %s; want one", count, target)
		}
	}
}

func TestPublishedCatalogReturnsIndependentCopies(t *testing.T) {
	before := PinnedManifest()
	if len(before.Releases) == 0 {
		t.Fatal("published catalog must be populated before testing copy isolation")
	}
	modified := PinnedManifest()
	modified.Releases[0].Version = "changed-by-caller"
	modified.Releases[0].SHA256 = ""
	modified.Releases = append(modified.Releases, Release{Version: "extra"})
	if after := PinnedManifest(); !reflect.DeepEqual(after, before) {
		t.Fatal("caller mutation changed the embedded release authority")
	}
}

func TestPublishedCatalogResolvesHostPinWithoutInstallation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("automatic official installation currently supports macOS only")
	}
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("no published Desktop artifact for this architecture")
	}
	manager := New(t.TempDir())
	release, err := manager.release()
	if err != nil {
		t.Fatalf("a source build cannot resolve its official host pin: %v", err)
	}
	if want := runtime.GOOS + "-" + runtime.GOARCH; release.Target != want {
		t.Fatalf("host pin target = %q, want %q", release.Target, want)
	}
}
