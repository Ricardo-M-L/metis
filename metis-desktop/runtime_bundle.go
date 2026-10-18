package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const desktopRuntimeMaxBytes int64 = 256 << 20

type desktopRuntimeManifest struct {
	SchemaVersion        int    `json:"schema_version"`
	Version              string `json:"version"`
	GOOS                 string `json:"goos"`
	GOARCH               string `json:"goarch"`
	SHA256               string `json:"sha256"`
	SizeBytes            int64  `json:"size_bytes"`
	Binary               string `json:"binary,omitempty"`
	Commit               string `json:"commit,omitempty"`
	BuiltAt              string `json:"built_at,omitempty"`
	SourceManifestSHA256 string `json:"source_manifest_sha256,omitempty"`
}
type desktopRuntimeSelection struct {
	Path     string
	Bundled  bool
	Manifest desktopRuntimeManifest
}
type desktopRuntimeResolver struct {
	executable string
	version    string
	goos       string
	goarch     string
	production bool
	payload    fs.FS
	cacheBase  string
	getenv     func(string) string
	legacy     func(string) (string, error)
}

func newDesktopRuntimeResolver(version string) *desktopRuntimeResolver {
	executable, _ := os.Executable()
	cache, _ := os.UserCacheDir()
	base := ""
	if cache != "" {
		base = filepath.Join(cache, "metis-desktop", "runtime")
	}
	return &desktopRuntimeResolver{executable: executable, version: version, goos: runtime.GOOS, goarch: runtime.GOARCH,
		production: desktopRuntimeProduction, payload: desktopRuntimePayloadFS, cacheBase: base, getenv: os.Getenv, legacy: findExternalMetisBinary}
}
func (a *App) resolveMetisRuntime() (desktopRuntimeSelection, error) {
	if a.resolveRuntime != nil {
		return a.resolveRuntime()
	}
	if a.findMetis == nil {
		return desktopRuntimeSelection{}, errors.New("Metis runtime resolver is unavailable")
	}
	selected, err := a.findMetis()
	if err != nil {
		return desktopRuntimeSelection{}, err
	}
	if a.runtimeResolver != nil && a.runtimeResolver.isOwnBundledPath(selected) {
		return a.runtimeResolver.requireBundled()
	}
	return desktopRuntimeSelection{Path: selected}, nil
}

func (r *desktopRuntimeResolver) resolve(preferred string) (desktopRuntimeSelection, error) {
	override := strings.TrimSpace(preferred)
	if override == "" && r.getenv != nil {
		override = strings.TrimSpace(r.getenv("METIS_BIN"))
	}
	if override != "" {
		if r.isOwnBundledPath(override) {
			return r.requireBundled()
		}
		abs, err := filepath.Abs(override)
		if err != nil {
			return desktopRuntimeSelection{}, err
		}
		info, err := os.Stat(abs)
		if err != nil || !executableMetisFile(abs, info, r.goos) {
			return desktopRuntimeSelection{}, fmt.Errorf("explicit Metis CLI is not executable: %s", abs)
		}
		return desktopRuntimeSelection{Path: abs}, nil
	}
	selection, present, err := r.bundled()
	if err != nil {
		return desktopRuntimeSelection{}, err
	}
	if present {
		return selection, nil
	}
	if r.production {
		return desktopRuntimeSelection{}, errors.New("Desktop package is missing its bundled Metis runtime; reinstall the complete Desktop application")
	}
	if r.legacy == nil {
		return desktopRuntimeSelection{}, errors.New("Metis CLI is unavailable in this development build")
	}
	external, err := r.legacy("")
	return desktopRuntimeSelection{Path: external}, err
}
func (r *desktopRuntimeResolver) bundled() (desktopRuntimeSelection, bool, error) {
	if r.goos == "darwin" {
		bundle := macDesktopBundlePath(r.executable)
		if bundle == "" {
			return desktopRuntimeSelection{}, false, nil
		}
		dir := filepath.Join(bundle, "Contents", "Resources", "bin")
		present := runtimePathExists(filepath.Join(dir, "metis-runtime.json")) || runtimePathExists(filepath.Join(dir, "metis"))
		if !present && !r.production {
			return desktopRuntimeSelection{}, false, nil
		}
		selection, err := verifyMacDesktopRuntime(bundle, r.version, r.goarch)
		return selection, true, err
	}
	if r.goos != "linux" && r.goos != "windows" {
		return desktopRuntimeSelection{}, false, nil
	}
	prefix := path.Join("embedded-runtime", r.goos+"-"+r.goarch)
	manifestPath := path.Join(prefix, "manifest.json")
	raw, err := readDesktopRuntimeFS(r.payload, manifestPath, 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return desktopRuntimeSelection{}, false, nil
	}
	if err != nil {
		return desktopRuntimeSelection{}, true, fmt.Errorf("read bundled Metis runtime manifest: %w", err)
	}
	manifest, err := decodeDesktopRuntimeManifest(raw, r.version, r.goos, r.goarch)
	if err != nil {
		return desktopRuntimeSelection{}, true, err
	}
	input, err := r.payload.Open(path.Join(prefix, runtimeBinaryName(r.goos)))
	if err != nil {
		return desktopRuntimeSelection{}, true, fmt.Errorf("Desktop package is missing its bundled Metis runtime payload: %w", err)
	}
	defer input.Close()
	if r.cacheBase == "" || !filepath.IsAbs(r.cacheBase) {
		return desktopRuntimeSelection{}, true, errors.New("a private user cache directory is required for the bundled Metis runtime")
	}
	binary, err := extractDesktopRuntime(input, manifest, r.cacheBase, r.goos)
	if err != nil {
		return desktopRuntimeSelection{}, true, err
	}
	return desktopRuntimeSelection{Path: binary, Bundled: true, Manifest: manifest}, true, nil
}
func verifyMacDesktopRuntime(bundle, version, goarch string) (desktopRuntimeSelection, error) {
	dir := filepath.Join(bundle, "Contents", "Resources", "bin")
	manifestPath := filepath.Join(dir, "metis-runtime.json")
	if err := noRuntimeSymlinks(bundle, manifestPath); err != nil {
		return desktopRuntimeSelection{}, err
	}
	raw, err := readDesktopRuntimeFile(manifestPath, 64<<10)
	if err != nil {
		return desktopRuntimeSelection{}, fmt.Errorf("Desktop package is missing or cannot read its bundled Metis runtime manifest: %w", err)
	}
	manifest, err := decodeDesktopRuntimeManifest(raw, version, "darwin", goarch)
	if err != nil {
		return desktopRuntimeSelection{}, err
	}
	binary := filepath.Join(dir, "metis")
	if err = noRuntimeSymlinks(bundle, binary); err != nil {
		return desktopRuntimeSelection{}, err
	}
	if err = verifyDesktopRuntimeFile(binary, manifest, "darwin"); err != nil {
		return desktopRuntimeSelection{}, err
	}
	return desktopRuntimeSelection{Path: binary, Bundled: true, Manifest: manifest}, nil
}
func decodeDesktopRuntimeManifest(raw []byte, version, goos, goarch string) (desktopRuntimeManifest, error) {
	var m desktopRuntimeManifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid bundled Metis runtime manifest: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, errors.New("bundled Metis runtime manifest must contain one JSON object")
	}
	universal := m.GOOS == "darwin" && m.GOARCH == "universal" && (goarch == "arm64" || goarch == "amd64")
	if m.SchemaVersion != 1 || !validDesktopVersion(m.Version) || normalizeDesktopVersion(m.Version) != normalizeDesktopVersion(version) || m.GOOS != goos || (!universal && m.GOARCH != goarch) || m.GOARCH == "universal" && m.GOOS != "darwin" || !runtimeDigest(m.SHA256) || m.SizeBytes <= 0 || m.SizeBytes > desktopRuntimeMaxBytes || m.Binary != "" && m.Binary != runtimeBinaryName(goos) {
		return m, errors.New("bundled Metis runtime manifest does not match this Desktop version, platform, binary or digest")
	}
	if m.SourceManifestSHA256 != "" && !runtimeDigest(m.SourceManifestSHA256) {
		return m, errors.New("invalid bundled runtime source digest")
	}
	return m, nil
}
func runtimeDigest(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func runtimeBinaryName(goos string) string {
	if goos == "windows" {
		return "metis.exe"
	}
	return "metis"
}
func runtimePathExists(p string) bool { _, err := os.Lstat(p); return !errors.Is(err, os.ErrNotExist) }
func macDesktopBundlePath(executable string) string {
	if executable == "" {
		return ""
	}
	abs, err := filepath.Abs(executable)
	if err != nil {
		return ""
	}
	macos := filepath.Dir(abs)
	contents := filepath.Dir(macos)
	bundle := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(strings.ToLower(filepath.Base(bundle)), ".app") {
		return ""
	}
	return bundle
}
func (r *desktopRuntimeResolver) isOwnBundledPath(selected string) bool {
	abs, err := filepath.Abs(selected)
	if err != nil {
		return false
	}
	candidates := []string{abs}
	if canonical, err := filepath.EvalSymlinks(abs); err == nil {
		candidates = append(candidates, canonical)
	}
	for _, p := range candidates {
		if r.goos == "darwin" {
			bundle := macDesktopBundlePath(r.executable)
			expected := filepath.Join(bundle, "Contents", "Resources", "bin", "metis")
			if bundle != "" && (filepath.Clean(p) == filepath.Clean(expected) || sameRuntimePath(p, expected)) {
				return true
			}
		}
		if r.cacheBase != "" && filepath.Base(p) == runtimeBinaryName(r.goos) && runtimeDigest(filepath.Base(filepath.Dir(p))) {
			base := filepath.Dir(filepath.Dir(p))
			if filepath.Clean(base) == filepath.Clean(r.cacheBase) || sameRuntimePath(base, r.cacheBase) {
				return true
			}
		}
	}
	return false
}
func sameRuntimePath(a, b string) bool {
	aInfo, ae := os.Stat(a)
	bInfo, be := os.Stat(b)
	return ae == nil && be == nil && os.SameFile(aInfo, bInfo)
}
func readDesktopRuntimeFS(fsys fs.FS, p string, limit int64) ([]byte, error) {
	if fsys == nil {
		return nil, fs.ErrNotExist
	}
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readRuntimeBounded(f, limit)
}
func readDesktopRuntimeFile(p string, limit int64) ([]byte, error) {
	f, err := openRegularRuntime(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readRuntimeBounded(f, limit)
}
func readRuntimeBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("bundled runtime metadata exceeds its capacity")
	}
	return b, nil
}
func noRuntimeSymlinks(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return errors.New("bundled runtime escaped its application directory")
	}
	current := root
	parts := strings.Split(relative, string(os.PathSeparator))
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			current = filepath.Join(current, parts[i])
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundled Metis runtime refuses a symbolic link: %s", current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("bundled runtime parent is not a directory")
		}
	}
	return nil
}
func openRegularRuntime(p string) (*os.File, error) {
	before, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("bundled runtime must be a regular file, not a symbolic link")
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, errors.New("bundled runtime file changed while opening")
	}
	return file, nil
}
func verifyDesktopRuntimeFile(p string, m desktopRuntimeManifest, goos string) error {
	f, err := openRegularRuntime(p)
	if err != nil {
		return fmt.Errorf("bundled Metis runtime is missing or unsafe: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != m.SizeBytes || !executableMetisFile(p, info, goos) {
		return errors.New("bundled Metis runtime size or executable permission does not match its manifest")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, m.SizeBytes+1))
	if err != nil || n != m.SizeBytes || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return errors.New("bundled Metis runtime checksum does not match its manifest; reinstall Desktop")
	}
	return nil
}

var desktopRuntimeCacheMu sync.Mutex

func extractDesktopRuntime(payload io.Reader, m desktopRuntimeManifest, base, goos string) (string, error) {
	desktopRuntimeCacheMu.Lock()
	defer desktopRuntimeCacheMu.Unlock()
	appCache := filepath.Dir(base)
	if err := os.MkdirAll(filepath.Dir(appCache), 0700); err != nil {
		return "", err
	}
	for _, dir := range []string{appCache, base, filepath.Join(base, m.SHA256)} {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("bundled Metis runtime cache refuses symbolic links and non-directories")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return "", errors.New("bundled Metis runtime cache must be private to the current user")
		}
	}
	dir := filepath.Join(base, m.SHA256)
	target := filepath.Join(dir, runtimeBinaryName(goos))
	if runtimePathExists(target) {
		if err := verifyDesktopRuntimeFile(target, m, goos); err != nil {
			return "", err
		}
		return target, nil
	}
	staged, err := os.CreateTemp(dir, ".metis-runtime-*")
	if err != nil {
		return "", err
	}
	temp := staged.Name()
	defer os.Remove(temp)
	h := sha256.New()
	if err = staged.Chmod(0700); err == nil {
		var n int64
		n, err = io.Copy(io.MultiWriter(staged, h), io.LimitReader(payload, m.SizeBytes+1))
		if err == nil && (n != m.SizeBytes || hex.EncodeToString(h.Sum(nil)) != m.SHA256) {
			err = errors.New("embedded Metis runtime checksum or size does not match its manifest")
		}
	}
	if err == nil {
		err = staged.Sync()
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		return "", err
	}
	// A hard link publishes without replacing an already-published runtime.
	// Concurrent Desktop windows verify and reuse the same immutable receipt.
	if err = os.Link(temp, target); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("publish bundled Metis runtime: %w", err)
	}
	if err = verifyDesktopRuntimeFile(target, m, goos); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		d, openErr := os.Open(dir)
		if openErr != nil {
			return "", openErr
		}
		syncErr := d.Sync()
		closeErr := d.Close()
		if err = errors.Join(syncErr, closeErr); err != nil {
			return "", err
		}
	}
	return target, nil
}

func desktopRuntimeChildEnvironment(environment []string, host string, bundled bool) []string {
	result := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "METIS_DESKTOP_BUNDLED_RUNTIME=") && !strings.HasPrefix(entry, "METIS_DESKTOP_APP=") && !(bundled && strings.HasPrefix(entry, "METIS_NO_UPDATE_CHECK=")) {
			result = append(result, entry)
		}
	}
	if bundled {
		if bundle := macDesktopBundlePath(host); bundle != "" {
			host = bundle
		}
		if abs, err := filepath.Abs(host); err == nil && host != "" {
			result = append(result, "METIS_DESKTOP_APP="+abs)
		}
		result = append(result, "METIS_DESKTOP_BUNDLED_RUNTIME=1", "METIS_NO_UPDATE_CHECK=1")
	}
	return result
}

func (r *desktopRuntimeResolver) requireBundled() (desktopRuntimeSelection, error) {
	selected, present, err := r.bundled()
	if err != nil {
		return selected, err
	}
	if !present || !selected.Bundled {
		return desktopRuntimeSelection{}, errors.New("Desktop package is missing its bundled Metis runtime; reinstall the complete Desktop application")
	}
	return selected, nil
}
