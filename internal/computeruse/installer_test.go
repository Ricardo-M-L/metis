package computeruse

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixtureDescription(version string) Description {
	return Description{Name: "metis-cu", Version: version, ProtocolVersion: ProtocolVersion, Platform: runtime.GOOS, Arch: runtime.GOARCH, Capabilities: []string{"screenshot", "status", "stop", "end-turn", "serialized-input", "input-ownership"}, Permissions: map[string]string{"screenRecording": "notGranted", "accessibility": "notGranted"}}
}

func descriptionScript(t *testing.T, description Description) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell executable fixtures require a Unix host")
	}
	data, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(string(data), "'", "'\"'\"'") + "'"
	return []byte("#!/bin/sh\n[ \"$1\" = --describe ] && [ \"$2\" = --json ] || exit 9\nprintf '%s\\n' " + quoted + "\n")
}

func fixtureExecutable(t *testing.T, description Description) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(filename, descriptionScript(t, description), 0700); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestProbeValidatesDescription(t *testing.T) {
	description := fixtureDescription("1.2.3")
	filename := fixtureExecutable(t, description)
	got, err := Probe(context.Background(), filename)
	if err != nil || got.Version != description.Version || got.Permissions["accessibility"] != "notGranted" {
		t.Fatalf("Probe = %+v, %v", got, err)
	}
	for _, test := range []struct {
		name  string
		alter func(*Description)
		want  string
	}{
		{"name", func(d *Description) { d.Name = "unrelated" }, "name"},
		{"protocol", func(d *Description) { d.ProtocolVersion++ }, "protocol"},
		{"platform", func(d *Description) { d.Platform = "other" }, "target"},
		{"arch", func(d *Description) { d.Arch = "other" }, "target"},
		{"version", func(d *Description) { d.Version = "" }, "version"},
		{"missing-stop", func(d *Description) {
			d.Capabilities = []string{"status", "end-turn", "serialized-input", "input-ownership"}
		}, "capability \"stop\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := description
			test.alter(&d)
			_, err := Probe(context.Background(), fixtureExecutable(t, d))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s rejection, got %v", test.want, err)
			}
		})
	}
}

func TestProbeBoundedAndCanceled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, test := range []struct {
		name, script, want  string
		timeout, maxElapsed time.Duration
	}{
		// Sandbox startup can be delayed when the whole repository is under
		// parallel test load. Keep these checks bounded without making a
		// healthy probe fail before it gets a chance to start.
		{"output", "#!/bin/sh\nhead -c 100000 /dev/zero\n", "output limit", 5 * time.Second, 8 * time.Second},
		{"timeout", "#!/bin/sh\nexec sleep 30\n", "canceled", 100 * time.Millisecond, 2 * time.Second},
		{"trailing", "#!/bin/sh\nprintf '%s\\n' '{} {}'\n", "trailing", 5 * time.Second, 8 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "helper")
			if err := os.WriteFile(filename, []byte(test.script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			defer cancel()
			start := time.Now()
			_, err := Probe(ctx, filename)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s, got %v", test.want, err)
			}
			if time.Since(start) > test.maxElapsed {
				t.Fatal("probe did not honor bounded execution")
			}
		})
	}
}

func TestInstallLocalPrivateCopyAndFailedReplacement(t *testing.T) {
	ctx := context.Background()
	m := New(t.TempDir())
	source := fixtureExecutable(t, fixtureDescription("1.0"))
	status, err := m.InstallLocal(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.Source != "local" || status.Path == source || status.Enabled || status.Running {
		t.Fatalf("unexpected status: %+v", status)
	}
	info, err := os.Stat(status.Path)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("private executable mode: %v, %v", info, err)
	}
	if err := os.WriteFile(source, []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Resolve(ctx); err != nil || got != status.Path {
		t.Fatalf("source change affected installed copy: %q, %v", got, err)
	}
	wrong := fixtureDescription("2.0")
	wrong.ProtocolVersion++
	if _, err := m.InstallLocal(ctx, fixtureExecutable(t, wrong)); err == nil {
		t.Fatal("incompatible helper installed")
	}
	if got, err := m.Resolve(ctx); err != nil || got != status.Path {
		t.Fatalf("failed replacement changed activation: %q, %v", got, err)
	}
	if err := os.Chmod(status.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(status.Path, []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(ctx); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered managed binary accepted: %v", err)
	}
}

func TestInstallLocalConcurrentActivation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	m := New(root)
	sources := []string{}
	for i := 0; i < 6; i++ {
		sources = append(sources, fixtureExecutable(t, fixtureDescription(fmt.Sprintf("1.%d", i))))
	}
	if _, err := m.InstallLocal(ctx, sources[0]); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 32)
	for _, source := range sources {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := New(root).InstallLocal(ctx, source)
			if err != nil {
				errorsSeen <- err
			}
		}()
	}
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			status, err := m.Status(ctx)
			if err != nil {
				errorsSeen <- err
				return
			}
			if !status.Installed {
				errorsSeen <- errors.New("activation disappeared during install")
				return
			}
		}
	}()
	wait.Wait()
	close(stop)
	<-readerDone
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	status, err := m.Status(ctx)
	if err != nil || !status.Installed {
		t.Fatalf("final activation: %+v, %v", status, err)
	}
	directory, _ := m.directory()
	versions, err := os.ReadDir(filepath.Join(directory, "versions"))
	if err != nil || len(versions) != len(sources) {
		t.Fatalf("expected immutable versions for all sources, count=%d err=%v", len(versions), err)
	}
}

func TestRejectSymlinksNonExecutableAndActivationTraversal(t *testing.T) {
	ctx := context.Background()
	source := fixtureExecutable(t, fixtureDescription("1.0"))
	symlink := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(source, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.TempDir()).InstallLocal(ctx, symlink); err == nil {
		t.Fatal("accepted source symlink")
	}
	if err := os.Chmod(source, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.TempDir()).InstallLocal(ctx, source); err == nil {
		t.Fatal("accepted non-executable source")
	}
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "components")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root).Status(ctx); err == nil {
		t.Fatal("accepted managed directory symlink")
	}
	m := New(t.TempDir())
	directory, _ := m.directory()
	if err := checkManagedDirectories(directory, true); err != nil {
		t.Fatal(err)
	}
	active := activation{Directory: "../../outside", BinaryPath: "metis-cu", SHA256: strings.Repeat("0", 64), Source: "local", Version: "1"}
	data, _ := json.Marshal(active)
	if err := os.WriteFile(filepath.Join(directory, "active.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(ctx); err == nil {
		t.Fatal("accepted activation traversal")
	}
}

func TestEmptyCatalogNeverFallsBackToPath(t *testing.T) {
	ctx := context.Background()
	source := fixtureExecutable(t, fixtureDescription("1.0"))
	if err := os.Rename(source, filepath.Join(filepath.Dir(source), "metis-cu")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(source))
	m := NewWithOptions(t.TempDir(), Options{Manifest: &Manifest{}})
	status, err := m.Ensure(ctx)
	if !errors.Is(err, ErrNoOfficialRelease) || status.Installed {
		t.Fatalf("empty catalog ensure = %+v, %v", status, err)
	}
	if _, err := m.Resolve(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("resolve used PATH: %v", err)
	}
}

type tarEntry struct {
	name string
	data []byte
	mode int64
	kind byte
	link string
}

func archiveBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: kind, Linkname: entry.link}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func testRelease(t *testing.T, artifact []byte) (*Manager, Release) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(artifact) }))
	t.Cleanup(server.Close)
	digest := sha256.Sum256(artifact)
	// Download-only tests do not execute their payload; installation tests replace
	// this pin with the actual executable's digest before constructing a manager.
	binaryDigest := sha256.Sum256(artifact)
	release := Release{Version: "1.2.3", ProtocolVersion: ProtocolVersion, Target: runtime.GOOS + "-" + runtime.GOARCH, URL: server.URL + "/metis-cu.tar.gz", SHA256: hex.EncodeToString(digest[:]), BinarySHA256: hex.EncodeToString(binaryDigest[:]), Format: "tar.gz", BinaryPath: "bundle/bin/metis-cu", Size: int64(len(artifact))}
	m := NewWithOptions(t.TempDir(), Options{Manifest: &Manifest{Releases: []Release{release}}, Client: server.Client()})
	return m, release
}

func TestDownloadRejectsCorruptChecksum(t *testing.T) {
	m, release := testRelease(t, []byte("corrupt release bytes"))
	release.SHA256 = strings.Repeat("0", 64)
	err := m.download(context.Background(), release, filepath.Join(t.TempDir(), "download"))
	if err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Fatalf("corrupt download = %v", err)
	}
	status, err := m.Status(context.Background())
	if err != nil || status.Installed {
		t.Fatalf("corrupt download activated: %+v %v", status, err)
	}
}

func TestOfficialInstallPreservesArchiveAndRejectsMismatch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("automatic official install is initially macOS-only")
	}
	for _, version := range []string{"1.2.3", "different"} {
		t.Run(version, func(t *testing.T) {
			artifact := archiveBytes(t, []tarEntry{{name: "bundle/bin/metis-cu", data: descriptionScript(t, fixtureDescription(version)), mode: 0755}, {name: "bundle/data/resource.txt", data: []byte("resource"), mode: 0644}})
			m, _ := testRelease(t, artifact)
			binaryDigest := sha256.Sum256(descriptionScript(t, fixtureDescription(version)))
			m.manifest.Releases[0].BinarySHA256 = hex.EncodeToString(binaryDigest[:])
			status, err := m.Ensure(context.Background())
			if version == "different" {
				if err == nil || !strings.Contains(err.Error(), "pinned version") {
					t.Fatalf("version mismatch accepted: %v", err)
				}
				return
			}
			if err != nil || !status.Installed || status.Source != "official" {
				t.Fatalf("official install: %+v %v", status, err)
			}
			resource, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(status.Path)), "data", "resource.txt"))
			if err != nil || string(resource) != "resource" {
				t.Fatalf("archive layout not preserved: %s %v", resource, err)
			}
		})
	}
}

func TestArchiveRejectsTraversalAndLinks(t *testing.T) {
	for _, entry := range []tarEntry{
		{name: "../escaped", data: []byte("bad"), mode: 0700},
		{name: "/escaped", data: []byte("bad"), mode: 0700},
		{name: "directory/../../escaped", data: []byte("bad"), mode: 0700},
		{name: "directory\\escaped", data: []byte("bad"), mode: 0700},
		{name: "link", kind: tar.TypeSymlink, link: "../escaped", mode: 0700},
		{name: "hardlink", kind: tar.TypeLink, link: "../escaped", mode: 0700},
	} {
		t.Run(entry.name, func(t *testing.T) {
			artifact := filepath.Join(t.TempDir(), "archive.tar.gz")
			if err := os.WriteFile(artifact, archiveBytes(t, []tarEntry{entry}), 0600); err != nil {
				t.Fatal(err)
			}
			if err := extractArtifact(context.Background(), artifact, t.TempDir(), Release{Format: "tar.gz"}); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, _ := writer.Create("../escaped")
	_, _ = entry.Write([]byte("bad"))
	_ = writer.Close()
	artifact := filepath.Join(t.TempDir(), "archive.zip")
	if err := os.WriteFile(artifact, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extractArtifact(context.Background(), artifact, t.TempDir(), Release{Format: "zip"}); err == nil {
		t.Fatal("accepted unsafe zip")
	}
}

func TestPinnedManifestValidation(t *testing.T) {
	_, release := testRelease(t, []byte("artifact"))
	for _, binaryPath := range []string{"../metis-cu", "/metis-cu", "a/../../metis-cu", "C:\\metis-cu", "a//metis-cu"} {
		candidate := release
		candidate.BinaryPath = binaryPath
		if err := validateRelease(candidate); err == nil {
			t.Errorf("accepted binaryPath %q", binaryPath)
		}
	}
	missingArchiveHash := release
	missingArchiveHash.SHA256 = ""
	if err := validateRelease(missingArchiveHash); err == nil {
		t.Fatal("accepted missing pinned checksum")
	}
	release.BinarySHA256 = ""
	if err := validateRelease(release); err == nil {
		t.Fatal("accepted missing pinned executable checksum")
	}
}

func TestLockHonorsCancellation(t *testing.T) {
	m := New(t.TempDir())
	unlock, _, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := m.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation = %v", err)
	}
}
