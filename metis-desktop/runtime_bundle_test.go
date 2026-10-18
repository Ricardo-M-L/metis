package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func writeRuntimeTestBundle(t *testing.T, version, arch string) (string, string, map[string]any) {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "Metis With Spaces.app")
	host := filepath.Join(bundle, "Contents", "MacOS", "metis-desktop")
	binary := filepath.Join(bundle, "Contents", "Resources", "bin", "metis")
	for _, p := range []string{host, binary} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(host, []byte("desktop fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("packaged cli fixture")
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(binary, payload, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"schema_version": 1, "version": version, "goos": "darwin", "goarch": arch, "sha256": hex.EncodeToString(sum[:]), "size_bytes": len(payload), "binary": "metis"}
	writeRuntimeTestManifest(t, filepath.Join(filepath.Dir(binary), "metis-runtime.json"), manifest)
	return host, binary, manifest
}
func writeRuntimeTestManifest(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBundledRuntimeWinsOverWorkspaceAndPATH(t *testing.T) {
	host, want, _ := writeRuntimeTestBundle(t, "1.2.3", "universal")
	r := desktopRuntimeResolver{executable: host, version: "1.2.3", goos: "darwin", goarch: "arm64", production: true, getenv: func(string) string { return "" }, legacy: func(string) (string, error) { return "/unrelated/workspace/metis", nil }}
	got, err := r.resolve("")
	if err != nil || got.Path != want || !got.Bundled {
		t.Fatalf("packaged Desktop selected external CLI: got=%+v err=%v want=%s", got, err, want)
	}
}

func runtimeTestResolver(t *testing.T, goos, arch string, payload []byte) (*desktopRuntimeResolver, fs.FS, desktopRuntimeManifest) {
	t.Helper()
	sum := sha256.Sum256(payload)
	manifest := desktopRuntimeManifest{SchemaVersion: 1, Version: "1.2.3", GOOS: goos, GOARCH: arch, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(payload)), Binary: runtimeBinaryName(goos)}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "embedded-runtime/" + goos + "-" + arch + "/"
	files := fstest.MapFS{prefix + "manifest.json": &fstest.MapFile{Data: raw}, prefix + runtimeBinaryName(goos): &fstest.MapFile{Data: payload}}
	r := &desktopRuntimeResolver{executable: filepath.Join(t.TempDir(), "metis-desktop"), version: "1.2.3", goos: goos, goarch: arch, production: true, payload: files,
		cacheBase: filepath.Join(t.TempDir(), "cache", "metis-desktop", "runtime"), getenv: func(string) string { return "" }, legacy: func(string) (string, error) { t.Fatal("bundled runtime reached external fallback"); return "", nil }}
	return r, files, manifest
}
func TestBundledRuntimeRejectsMissingProductionPayloadAndAllowsDevLegacy(t *testing.T) {
	calls := 0
	r := desktopRuntimeResolver{version: "1.2.3", goos: "linux", goarch: "amd64", production: true, payload: fstest.MapFS{}, getenv: func(string) string { return "" }, legacy: func(string) (string, error) { calls++; return "/legacy/metis", nil }}
	if _, err := r.resolve(""); err == nil || calls != 0 {
		t.Fatalf("missing production runtime fell back: %v calls %d", err, calls)
	}
	r.production = false
	got, err := r.resolve("")
	if err != nil || got.Path != "/legacy/metis" || got.Bundled || calls != 1 {
		t.Fatalf("legacy dev = %+v %v calls %d", got, err, calls)
	}
}
func TestBundledRuntimeExplicitExternalOverrideRemainsAvailable(t *testing.T) {
	host, binary, _ := writeRuntimeTestBundle(t, "1.2.3", "arm64")
	if err := os.WriteFile(binary, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "metis")
	if err := os.WriteFile(external, []byte("external fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	r := desktopRuntimeResolver{executable: host, version: "1.2.3", goos: "darwin", goarch: "arm64", production: true, getenv: func(string) string { return "" }}
	if _, err := r.resolve(""); err == nil {
		t.Fatal("damaged packaged runtime silently fell back")
	}
	got, err := r.resolve(external)
	if err != nil || got.Path != external || got.Bundled {
		t.Fatalf("explicit developer override = %+v %v", got, err)
	}
	r.getenv = func(name string) string {
		if name == "METIS_BIN" {
			return external
		}
		return ""
	}
	got, err = r.resolve("")
	if err != nil || got.Path != external || got.Bundled {
		t.Fatalf("env developer override = %+v %v", got, err)
	}
}
func TestBundledMacRuntimeManifestAndFileValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"schema", func(m map[string]any) { m["schema_version"] = 2 }},
		{"version", func(m map[string]any) { m["version"] = "1.2.4" }},
		{"platform", func(m map[string]any) { m["goos"] = "linux" }},
		{"architecture", func(m map[string]any) { m["goarch"] = "amd64" }},
		{"checksum", func(m map[string]any) { m["sha256"] = strings.Repeat("a", 64) }},
		{"size", func(m map[string]any) { m["size_bytes"] = 1 }},
		{"binary", func(m map[string]any) { m["binary"] = "../metis" }},
		{"unknown field", func(m map[string]any) { m["future_executable"] = "/arbitrary" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, binary, m := writeRuntimeTestBundle(t, "1.2.3", "arm64")
			tc.change(m)
			writeRuntimeTestManifest(t, filepath.Join(filepath.Dir(binary), "metis-runtime.json"), m)
			if _, err := verifyMacDesktopRuntime(macDesktopBundlePath(host), "1.2.3", "arm64"); err == nil {
				t.Fatal("accepted mismatched runtime manifest")
			}
		})
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run("universal-"+arch, func(t *testing.T) {
			host, _, _ := writeRuntimeTestBundle(t, "1.2.3", "universal")
			if _, err := verifyMacDesktopRuntime(macDesktopBundlePath(host), "1.2.3", arch); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestBundledMacRuntimeRejectsSymlinkComponentsAndNonExecutable(t *testing.T) {
	for _, component := range []string{"metis", "metis-runtime.json", "bin"} {
		t.Run(component, func(t *testing.T) {
			host, binary, _ := writeRuntimeTestBundle(t, "1.2.3", "arm64")
			target := binary
			if component == "metis-runtime.json" {
				target = filepath.Join(filepath.Dir(binary), component)
			}
			if component == "bin" {
				target = filepath.Dir(binary)
			}
			displaced := target + ".original"
			if err := os.Rename(target, displaced); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(displaced, target); err != nil {
				t.Skip(err)
			}
			if _, err := verifyMacDesktopRuntime(macDesktopBundlePath(host), "1.2.3", "arm64"); err == nil {
				t.Fatal("accepted symlinked runtime component")
			}
		})
	}
	if runtime.GOOS != "windows" {
		host, binary, _ := writeRuntimeTestBundle(t, "1.2.3", "arm64")
		if err := os.Chmod(binary, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyMacDesktopRuntime(macDesktopBundlePath(host), "1.2.3", "arm64"); err == nil {
			t.Fatal("accepted non-executable mac runtime")
		}
	}
}
func TestBundledEmbeddedRuntimeCachesAndRevalidatesImmutablePayload(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			r, _, m := runtimeTestResolver(t, goos, "amd64", []byte("immutable payload fixture"))
			first, err := r.resolve("")
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(r.cacheBase, m.SHA256, runtimeBinaryName(goos))
			if first.Path != want || !first.Bundled {
				t.Fatalf("cache = %+v", first)
			}
			second, err := r.resolve("")
			if err != nil || second.Path != first.Path {
				t.Fatalf("reuse = %+v %v", second, err)
			}
			if err = os.WriteFile(first.Path, []byte("tampered"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err = r.resolve(""); err == nil {
				t.Fatal("tampered cache executed/repaired without explicit review")
			}
			data, _ := os.ReadFile(first.Path)
			if string(data) != "tampered" {
				t.Fatal("existing cache was overwritten")
			}
		})
	}
}
func TestBundledEmbeddedRuntimeRejectsDamagedPayloadAndCacheSymlinks(t *testing.T) {
	r, files, m := runtimeTestResolver(t, "linux", "amd64", []byte("expected payload"))
	files.(fstest.MapFS)["embedded-runtime/linux-amd64/metis"].Data = []byte("corrupt payload")
	if _, err := r.resolve(""); err == nil {
		t.Fatal("accepted corrupted embedded bytes")
	}
	if runtimePathExists(filepath.Join(r.cacheBase, m.SHA256, "metis")) {
		t.Fatal("published partial corrupt payload")
	}
	for _, where := range []string{"base", "digest", "binary"} {
		t.Run(where, func(t *testing.T) {
			r, _, m := runtimeTestResolver(t, "linux", "amd64", []byte("expected payload"))
			if err := os.MkdirAll(r.cacheBase, 0700); err != nil {
				t.Fatal(err)
			}
			target := r.cacheBase
			if where != "base" {
				target = filepath.Join(r.cacheBase, m.SHA256)
			}
			if where == "binary" {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(target, "metis")
			}
			if where == "base" {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			}
			outside := t.TempDir()
			if where == "binary" {
				outside = filepath.Join(outside, "metis")
				if err := os.WriteFile(outside, []byte("outside"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Skip(err)
			}
			if _, err := r.resolve(""); err == nil {
				t.Fatal("accepted symlinked runtime cache")
			}
		})
	}
}
func TestBundledEmbeddedRuntimeConcurrentWindowsReuseAndOldPreferredRebind(t *testing.T) {
	r, _, m := runtimeTestResolver(t, "windows", "amd64", []byte("shared fixture"))
	results := make(chan desktopRuntimeSelection, 12)
	errs := make(chan error, 12)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() { defer group.Done(); selection, err := r.resolve(""); results <- selection; errs <- err }()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(r.cacheBase, m.SHA256, "metis.exe")
	for selection := range results {
		if selection.Path != want || !selection.Bundled {
			t.Fatalf("concurrent selection %+v", selection)
		}
	}
	old := filepath.Join(r.cacheBase, strings.Repeat("a", 64), "metis.exe")
	got, err := r.resolve(old)
	if err != nil || got.Path != want || !got.Bundled {
		t.Fatalf("old bundled preferred pinned wrong version: %+v %v", got, err)
	}
	paths, _ := filepath.Glob(filepath.Join(r.cacheBase, m.SHA256, ".metis-runtime-*"))
	if len(paths) != 0 {
		t.Fatalf("staging files survived: %v", paths)
	}
}
func TestDesktopRuntimeChildEnvironmentOwnsBundleIdentity(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "Metis.app", "Contents", "MacOS", "metis-desktop")
	environment := []string{"KEEP=value", "METIS_DESKTOP_APP=/old/app", "METIS_DESKTOP_BUNDLED_RUNTIME=1", "METIS_NO_UPDATE_CHECK=0"}
	selected := desktopRuntimeChildEnvironment(environment, outer, true)
	joined := strings.Join(selected, "\n")
	if strings.Contains(joined, "/old/app") || !strings.Contains(joined, "METIS_DESKTOP_APP="+macDesktopBundlePath(outer)) || !strings.Contains(joined, "METIS_DESKTOP_BUNDLED_RUNTIME=1") || !strings.Contains(joined, "METIS_NO_UPDATE_CHECK=1") {
		t.Fatalf("child identity %v", selected)
	}
	external := strings.Join(desktopRuntimeChildEnvironment(environment, outer, false), "\n")
	if strings.Contains(external, "METIS_DESKTOP_APP=") || strings.Contains(external, "METIS_DESKTOP_BUNDLED_RUNTIME=") {
		t.Fatalf("external child inherited bundled identity: %s", external)
	}
}

func TestBundledRuntimeCacheProcessHelper(t *testing.T) {
	if os.Getenv("METIS_RUNTIME_CACHE_HELPER") != "1" {
		return
	}
	payload := []byte("cross-process immutable fixture")
	sum := sha256.Sum256(payload)
	m := desktopRuntimeManifest{SchemaVersion: 1, Version: "1.2.3", GOOS: "linux", GOARCH: "amd64", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(payload)), Binary: "metis"}
	raw, _ := json.Marshal(m)
	r := desktopRuntimeResolver{version: "1.2.3", goos: "linux", goarch: "amd64", production: true, cacheBase: os.Getenv("METIS_RUNTIME_CACHE_BASE"), getenv: func(string) string { return "" }, payload: fstest.MapFS{
		"embedded-runtime/linux-amd64/metis": &fstest.MapFile{Data: payload}, "embedded-runtime/linux-amd64/manifest.json": &fstest.MapFile{Data: raw}}}
	selected, err := r.requireBundled()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, selected.Path)
	os.Exit(0)
}
func TestBundledRuntimeCachePublishesAtomicallyAcrossProcesses(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "cache", "metis-desktop", "runtime")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var group sync.WaitGroup
	outputs := make(chan string, 6)
	failures := make(chan error, 6)
	for i := 0; i < 6; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestBundledRuntimeCacheProcessHelper$")
			cmd.Env = append(os.Environ(), "METIS_RUNTIME_CACHE_HELPER=1", "METIS_RUNTIME_CACHE_BASE="+base)
			output, err := cmd.CombinedOutput()
			outputs <- strings.TrimSpace(string(output))
			failures <- err
		}()
	}
	group.Wait()
	close(outputs)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var target string
	for output := range outputs {
		if target == "" {
			target = output
		}
		if output != target {
			t.Fatalf("processes selected different runtimes: %q != %q", output, target)
		}
	}
	payload, err := os.ReadFile(target)
	if err != nil || string(payload) != "cross-process immutable fixture" {
		t.Fatalf("published payload %q: %v", payload, err)
	}
	staged, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".metis-runtime-*"))
	if len(staged) != 0 {
		t.Fatalf("cross-process staging leaked: %v", staged)
	}
}
