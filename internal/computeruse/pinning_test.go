package computeruse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func officialTestManager(t *testing.T, root, version string, executable []byte) *Manager {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("official installation is initially macOS-only")
	}
	artifact := archiveBytes(t, []tarEntry{{name: "bundle/bin/metis-cu", data: executable, mode: 0755}})
	m, _ := testRelease(t, artifact)
	m.root = root
	m.manifest.Releases[0].Version = version
	digest := sha256.Sum256(executable)
	m.manifest.Releases[0].BinarySHA256 = hex.EncodeToString(digest[:])
	return m
}

func TestOfficialResolutionUsesEachBuildsOwnPin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a := officialTestManager(t, root, "version-a", descriptionScript(t, fixtureDescription("version-a")))
	b := officialTestManager(t, root, "version-b", descriptionScript(t, fixtureDescription("version-b")))
	statusA, err := a.Ensure(ctx)
	if err != nil || !statusA.Installed {
		t.Fatalf("install A: %+v, %v", statusA, err)
	}
	beforeB, err := b.Status(ctx)
	if err != nil || beforeB.Installed {
		t.Fatalf("build B incorrectly accepted A as installed: %+v, %v", beforeB, err)
	}
	statusB, err := b.Ensure(ctx)
	if err != nil || !statusB.Installed || statusB.Version != "version-b" || statusB.Path == statusA.Path {
		t.Fatalf("build B failed to install its own immutable version: %+v, %v", statusB, err)
	}
	for _, test := range []struct {
		name    string
		manager *Manager
		want    string
	}{{"A", a, statusA.Path}, {"B", b, statusB.Path}} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.manager.Resolve(ctx)
			if err != nil || got != test.want {
				t.Fatalf("resolve = %q, %v; want own pin %q", got, err, test.want)
			}
			ensured, err := test.manager.Ensure(ctx)
			if err != nil || ensured.Path != test.want {
				t.Fatalf("ensure = %+v, %v; want own pin %q", ensured, err, test.want)
			}
		})
	}
}

func TestOfficialActivationCannotCreateTrustWithoutPin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager := officialTestManager(t, root, "official", descriptionScript(t, fixtureDescription("official")))
	if _, err := manager.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWithOptions(root, Options{Manifest: &Manifest{}}).Resolve(ctx); !errors.Is(err, ErrNoOfficialRelease) {
		t.Fatalf("unpinned METIS build accepted official activation: %v", err)
	}
}

func TestOfficialResolutionRejectsSelfDeclaredReplacementHash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager := officialTestManager(t, root, "official", descriptionScript(t, fixtureDescription("official")))
	installed, err := manager.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	forged := append(descriptionScript(t, fixtureDescription("official")), []byte("touch '"+strings.ReplaceAll(marker, "'", "'\"'\"'")+"'\n")...)
	if err := os.Chmod(installed.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed.Path, forged, 0700); err != nil {
		t.Fatal(err)
	}
	active := officialActivation(manager.manifest.Releases[0])
	digest := sha256.Sum256(forged)
	active.SHA256 = hex.EncodeToString(digest[:])
	data, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := manager.directory()
	if err := os.WriteFile(filepath.Join(directory, "active.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(ctx); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("accepted self-declared replacement hash: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered binary executed before pinned-hash verification: %v", err)
	}
}

func TestOfficialInstallChecksExecutablePinBeforeProbe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	binary := append(descriptionScript(t, fixtureDescription("official")), []byte("touch '"+strings.ReplaceAll(marker, "'", "'\"'\"'")+"'\n")...)
	manager := officialTestManager(t, t.TempDir(), "official", binary)
	manager.manifest.Releases[0].BinarySHA256 = strings.Repeat("0", 64)
	if _, err := manager.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned binarySha256") {
		t.Fatalf("accepted executable inconsistent with trusted pin: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified executable was probed: %v", err)
	}
}

func TestExplicitLocalSelectionStillWorksWithDifferentOfficialPin(t *testing.T) {
	root := t.TempDir()
	manager := officialTestManager(t, root, "official", descriptionScript(t, fixtureDescription("official")))
	local, err := manager.InstallLocal(context.Background(), fixtureExecutable(t, fixtureDescription("local-opt-in")))
	if err != nil {
		t.Fatal(err)
	}
	ensured, err := manager.Ensure(context.Background())
	if err != nil || ensured.Source != "local" || ensured.Path != local.Path {
		t.Fatalf("explicit local selection was replaced: %+v, %v", ensured, err)
	}
}

func TestInstallLocalRejectsMissingStopBeforeActivation(t *testing.T) {
	description := fixtureDescription("missing-stop")
	description.Capabilities = []string{"status", "end-turn", "serialized-input", "input-ownership"}
	manager := New(t.TempDir())
	if _, err := manager.InstallLocal(context.Background(), fixtureExecutable(t, description)); err == nil || !strings.Contains(err.Error(), "capability \"stop\"") {
		t.Fatalf("installed helper without stop lifecycle guarantee: %v", err)
	}
	status, err := manager.Status(context.Background())
	if err != nil || status.Installed {
		t.Fatalf("rejected helper became active: %+v, %v", status, err)
	}
}
