// Package update implements self-update for Metis from GitHub releases.
// Public releases work anonymously; METIS_GITHUB_TOKEN (falling back to
// GITHUB_TOKEN) is optional and raises the GitHub API rate limit.
package update

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
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

const (
	defaultRepo           = "Ricardo-M-L/metis"
	userAgent             = "metis-self-update"
	checkTimeout          = 10 * time.Second
	dlTimeout             = 5 * time.Minute
	verifyTimeout         = 15 * time.Second
	maxArchiveSize  int64 = 128 << 20
	maxExpandedSize int64 = 128 << 20
	maxVerifyOutput       = 64 << 10
)

// apiBase is the GitHub API root. A var (not const) so apply_test.go can
// point Apply at an httptest server instead of the real api.github.com.
var (
	apiBase = "https://api.github.com"
	webBase = "https://github.com"
)

// Repo identifies the GitHub owner/repo to query. Override via METIS_REPO.
func Repo() string {
	if v := strings.TrimSpace(os.Getenv("METIS_REPO")); v != "" {
		return v
	}
	return defaultRepo
}

// Token returns an optional token for GitHub API requests, or "" if none is
// available. Public Metis releases can be checked and downloaded anonymously;
// authentication raises GitHub's API rate limit and also supports a private
// METIS_REPO override.
//
// Resolution order (first non-empty wins):
//  1. METIS_GITHUB_TOKEN env var (explicit, scoped to metis)
//  2. GITHUB_TOKEN env var (CI / shared)
//  3. `gh auth token` shell command (gh CLI's keyring) — zero-config
//     when the user is already logged into gh, which covers most
//     dev machines. We deliberately do NOT call this from any tight
//     loop; ghAuthToken caches per-process so the spawn cost is paid
//     at most once per metis run.
//
// The gh fallback is best-effort: not having gh installed or logged in is a
// normal anonymous-public-release path, not an error.
func Token() string {
	if v := strings.TrimSpace(os.Getenv("METIS_GITHUB_TOKEN")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); v != "" {
		return v
	}
	return ghAuthToken()
}

// ghAuthTokenCache memoizes the result of `gh auth token` so a hot
// startup doesn't shell-out repeatedly. We accept that the cache is
// stale across process lifetime — token rotation is rare, and a
// stale token just falls back to "current only" gracefully on next
// MaybeCheck.
var (
	ghAuthTokenCached    string
	ghAuthTokenLookedUp  bool
	ghAuthTokenLookupErr error
)

// ghAuthToken returns the gh CLI's stored token, or "" if gh isn't
// installed / not logged in / the keyring lookup failed. Failures
// are silent — this is a zero-config best-effort path; bubbling
// errors here would just spam stderr on machines that legitimately
// don't use gh.
func ghAuthToken() string {
	if ghAuthTokenLookedUp {
		return ghAuthTokenCached
	}
	ghAuthTokenLookedUp = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		ghAuthTokenLookupErr = err
		return ""
	}
	ghAuthTokenCached = strings.TrimSpace(out.String())
	return ghAuthTokenCached
}

type asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	Assets     []asset `json:"assets"`
}

// Latest fetches the latest non-draft release.
func Latest(ctx context.Context, token string) (*release, error) {
	// Anonymous public updates intentionally avoid the shared-IP GitHub REST
	// limit (60 requests/hour). The stable web redirect reveals the tag; Metis
	// release asset names are deterministic, so no REST asset IDs are needed.
	if strings.TrimSpace(token) == "" {
		if r, err := latestFromPublicWeb(ctx); err == nil {
			return r, nil
		}
		// Fall through to anonymous REST for GitHub-compatible mirrors that do
		// not expose the standard web redirect.
	}
	return latestFromAPI(ctx, token)
}

func latestFromAPI(ctx context.Context, token string) (*release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, Repo())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setAuth(req, token, "application/vnd.github+json")

	client := &http.Client{Timeout: checkTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("github API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func latestFromPublicWeb(ctx context.Context) (*release, error) {
	latestURL := strings.TrimRight(webBase, "/") + "/" + strings.Trim(Repo(), "/") + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	setAuth(req, "", "text/html")
	resp, err := (&http.Client{Timeout: checkTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases web %d", resp.StatusCode)
	}
	prefix := "/" + strings.Trim(Repo(), "/") + "/releases/tag/"
	if !strings.HasPrefix(resp.Request.URL.Path, prefix) {
		return nil, fmt.Errorf("latest release redirect did not resolve to a tag: %s", resp.Request.URL.Path)
	}
	escapedTag := strings.TrimPrefix(resp.Request.URL.Path, prefix)
	tag, err := url.PathUnescape(escapedTag)
	if err != nil || tag == "" || strings.Contains(tag, "/") {
		return nil, fmt.Errorf("invalid latest release tag %q", escapedTag)
	}
	if _, err := normalizeVersion(tag); err != nil {
		return nil, err
	}
	return deterministicPublicRelease(tag), nil
}

func deterministicPublicRelease(tag string) *release {
	target := Target()
	extension := ".tar.gz"
	if strings.HasPrefix(target, "windows-") {
		extension = ".zip"
	}
	name := "metis-" + target + extension
	base := strings.TrimRight(webBase, "/") + "/" + strings.Trim(Repo(), "/") + "/releases/download/" + url.PathEscape(tag) + "/"
	return &release{
		TagName: tag,
		HTMLURL: strings.TrimRight(webBase, "/") + "/" + strings.Trim(Repo(), "/") + "/releases/tag/" + url.PathEscape(tag),
		Assets: []asset{
			{Name: name, BrowserDownloadURL: base + url.PathEscape(name)},
			{Name: name + ".sha256", BrowserDownloadURL: base + url.PathEscape(name+".sha256")},
		},
	}
}

// targetForTest overrides Target()'s platform tag in tests. Empty in
// production; set via t.Cleanup-restored package var in apply_test.go so
// Apply can be exercised against a fake "test-os-arch" release without
// cross-compiling fixtures for the host platform.
var targetForTest = ""

// Target returns the platform tag (e.g. "darwin-arm64") used in asset names.
func Target() string {
	if targetForTest != "" {
		return targetForTest
	}
	return fmt.Sprintf("%s-%s", goruntime.GOOS, goruntime.GOARCH)
}

// findAsset returns the asset whose name matches `metis-<target>.tar.gz`
// and its sha256 sibling.
func (r *release) findAsset(target string) (binary, sum *asset, err error) {
	extension := ".tar.gz"
	if strings.HasPrefix(target, "windows-") {
		extension = ".zip"
	}
	wantBin := fmt.Sprintf("metis-%s%s", target, extension)
	wantSum := wantBin + ".sha256"
	for i := range r.Assets {
		a := &r.Assets[i]
		switch a.Name {
		case wantBin:
			binary = a
		case wantSum:
			sum = a
		}
	}
	if binary == nil {
		return nil, nil, fmt.Errorf("release %s has no asset %q", r.TagName, wantBin)
	}
	if sum == nil {
		return nil, nil, fmt.Errorf("release %s has no asset %q", r.TagName, wantSum)
	}
	return binary, sum, nil
}

// Apply downloads the release for the current platform, verifies its
// sha256, smoke-tests its reported version, and installs it using a versioned
// layout that mirrors Claude Code's native self-update:
//
//	Unix:    <prefix>/share/metis/versions/<semver>/metis + bin/metis symlink
//	Windows: <install-root>/versions/<semver>/metis.exe + bin/metis.exe copy
//
// Unix atomically renames a temporary symlink. Windows first renames the
// visible executable to a rollback name, copies the verified immutable
// version, and rolls back on failure. Both paths share the same cross-process
// lock and retain current plus the two newest unprotected rollback versions.
//
// destPath is always the stable user-facing launcher returned by SelfPath;
// an immutable versions/<version> target is normalized back to that launcher
// only when the managed relationship can be verified.
func Apply(ctx context.Context, token, destPath string, r *release) error {
	_, err := apply(ctx, token, destPath, r, false)
	return err
}

// ApplyIfNeeded is the automatic-update variant. It avoids downloading the
// same release after another process has already activated it while this
// caller waited on the shared install lock. Manual `metis update --force`
// continues to use Apply and therefore really reinstalls.
func ApplyIfNeeded(ctx context.Context, token, destPath string, r *release) (bool, error) {
	return apply(ctx, token, destPath, r, true)
}

func apply(ctx context.Context, token, destPath string, r *release, skipCurrent bool) (bool, error) {
	_, layout, ok := managedLayoutForApply(destPath)
	if !ok {
		return false, fmt.Errorf("refusing to treat managed version target %q as launcher", destPath)
	}
	releaseLock, err := acquireInstallLock(ctx, layout)
	if err != nil {
		return false, err
	}
	defer releaseLock()
	if err := reconcileActivation(layout); err != nil {
		return false, fmt.Errorf("recover interrupted activation: %w", err)
	}
	cleanupStaging(layout.stagingRoot, time.Now().Add(-stagingMaxAge))
	cleanupPlatformTemps(layout, time.Now().Add(-stagingMaxAge).UnixNano())
	cleanupStaleLockArtifacts(layout.locksRoot, time.Now().Add(-staleLockArtifactAge))

	if r == nil {
		r, err = Latest(ctx, token)
		if err != nil {
			return false, err
		}
	}
	version, err := normalizeVersion(r.TagName)
	if err != nil {
		return false, err
	}
	// Another process may have completed this exact update while this caller
	// waited for the cross-process lock.
	if current, currentOK := resolveCurrentVersion(layout); skipCurrent && currentOK && current == version {
		return false, cleanupManagedLocked(layout, time.Now())
	}
	target := Target()
	binAsset, sumAsset, err := r.findAsset(target)
	if err != nil {
		return false, err
	}
	if binAsset.Size > maxArchiveSize {
		return false, fmt.Errorf("release archive is too large: %d bytes (limit %d)", binAsset.Size, maxArchiveSize)
	}

	// All staging lives below managedRoot so the final version-directory rename
	// is same-filesystem and atomic. Old staging is cleaned only by age.
	if err := ensureDirectDirectory(layout.stagingRoot, 0o755); err != nil {
		return false, fmt.Errorf("create staging root: %w", err)
	}
	nonce, err := randomNonce()
	if err != nil {
		return false, err
	}
	stageDir := filepath.Join(layout.stagingRoot, "install-"+nonce)
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return false, fmt.Errorf("create update staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	archivePath := filepath.Join(stageDir, binAsset.Name)
	if err := downloadAsset(ctx, token, binAsset, archivePath); err != nil {
		return false, fmt.Errorf("download release archive: %w", err)
	}

	wantSum, err := fetchSum(ctx, token, sumAsset, binAsset.Name)
	if err != nil {
		return false, fmt.Errorf("fetch sha256: %w", err)
	}
	gotSum, err := sha256File(archivePath)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(gotSum, wantSum) {
		return false, fmt.Errorf("sha256 mismatch: got %s want %s", gotSum, wantSum)
	}

	innerName := fmt.Sprintf("metis-%s", target)
	if strings.HasPrefix(target, "windows-") {
		innerName = "metis.exe"
	}
	binPath, err := extractReleaseBinary(archivePath, stageDir, innerName)
	if err != nil {
		return false, fmt.Errorf("extract: %w", err)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return false, err
	}
	if targetForTest == "" {
		if err := verifyCandidate(ctx, binPath, version); err != nil {
			return false, fmt.Errorf("verify downloaded binary: %w", err)
		}
	}

	if err := ensureDirectDirectory(layout.versionsRoot, 0o755); err != nil {
		return false, fmt.Errorf("create versions root: %w", err)
	}
	if err := migrateLegacyLauncher(ctx, layout); err != nil {
		return false, err
	}
	versionStage := filepath.Join(layout.stagingRoot, "version-"+version+"-"+nonce)
	if err := os.Mkdir(versionStage, 0o755); err != nil {
		return false, fmt.Errorf("create version staging directory: %w", err)
	}
	defer os.RemoveAll(versionStage)
	stagedBinary := filepath.Join(versionStage, executableName())
	if err := moveFile(binPath, stagedBinary); err != nil {
		return false, fmt.Errorf("stage versioned binary: %w", err)
	}
	versionedBin, err := installImmutableVersion(layout, version, versionStage, stagedBinary)
	if err != nil {
		return false, err
	}
	if err := activateVersion(layout, version, versionedBin); err != nil {
		return false, err
	}
	// Never prune before the candidate has passed checksum/health validation
	// and the launcher switch has succeeded. Cleanup failure does not roll back
	// an already-atomic successful activation.
	_ = cleanupManagedLocked(layout, time.Now())
	return true, nil
}

func migrateLegacyLauncher(ctx context.Context, layout installLayout) error {
	if _, managed := resolveCurrentVersion(layout); managed {
		return nil
	}
	info, err := os.Lstat(layout.launcher)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace unrecognized launcher %s", layout.launcher)
	}
	legacyVersion, output, err := reportedBinaryVersion(ctx, layout.launcher)
	if err != nil {
		return fmt.Errorf("verify legacy launcher before migration: %w: %s", err, output)
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	stage := filepath.Join(layout.stagingRoot, "legacy-"+legacyVersion+"-"+nonce)
	if err := os.Mkdir(stage, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stagedBinary := filepath.Join(stage, executableName())
	if err := copyFile(layout.launcher, stagedBinary, info.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve legacy launcher: %w", err)
	}
	_ = os.Chtimes(stagedBinary, info.ModTime(), info.ModTime())
	if _, err := installImmutableVersion(layout, legacyVersion, stage, stagedBinary); err != nil {
		return fmt.Errorf("preserve legacy rollback version: %w", err)
	}
	return nil
}

func installImmutableVersion(layout installLayout, version, stagedDir, stagedBinary string) (string, error) {
	finalDir := filepath.Join(layout.versionsRoot, version)
	finalBinary := versionBinary(layout, version)
	info, err := os.Lstat(finalDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(stagedDir, finalDir); err != nil {
			return "", fmt.Errorf("install immutable version directory: %w", err)
		}
		return finalBinary, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("version path %s is not a regular directory", finalDir)
	}
	binInfo, err := os.Lstat(finalBinary)
	if err != nil || !binInfo.Mode().IsRegular() || binInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("existing version %s is not a regular immutable binary", version)
	}
	if !sameFileContents(stagedBinary, finalBinary) {
		return "", fmt.Errorf("existing immutable version %s differs from downloaded release", version)
	}
	return finalBinary, nil
}

// moveFile renames src to dst, falling back to copy+delete across
// filesystems. dst's parent must already exist.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		return err
	}
	return os.Remove(src)
}

func downloadAsset(ctx context.Context, token string, a *asset, dest string) error {
	if a == nil {
		return fmt.Errorf("missing release asset")
	}
	if a.Size > maxArchiveSize {
		return fmt.Errorf("asset %s is too large", a.Name)
	}
	url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", apiBase, Repo(), a.ID)
	if strings.TrimSpace(token) == "" && a.BrowserDownloadURL != "" {
		url = a.BrowserDownloadURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	setAuth(req, token, "application/octet-stream")

	client := &http.Client{Timeout: dlTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.ContentLength > maxArchiveSize {
		return fmt.Errorf("asset %s is too large: %d bytes", a.Name, resp.ContentLength)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxArchiveSize+1))
	if err != nil {
		return err
	}
	if n > maxArchiveSize {
		return fmt.Errorf("asset %s exceeds %d bytes", a.Name, maxArchiveSize)
	}
	return f.Close()
}

// fetchSum reads the .sha256 sidecar file and returns the hex digest for the
// given filename. shasum format: "<hex>  <name>\n".
func fetchSum(ctx context.Context, token string, a *asset, wantName string) (string, error) {
	if a == nil {
		return "", fmt.Errorf("missing checksum asset")
	}
	url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", apiBase, Repo(), a.ID)
	if strings.TrimSpace(token) == "" && a.BrowserDownloadURL != "" {
		url = a.BrowserDownloadURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	setAuth(req, token, "application/octet-stream")

	resp, err := (&http.Client{Timeout: checkTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("github API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		// `shasum -a 256` writes "<hex>  <name>"; the name may have a leading "*".
		name := strings.TrimPrefix(parts[len(parts)-1], "*")
		if name == wantName || filepath.Base(name) == wantName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("sha256 sidecar has no entry for %s", wantName)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractReleaseBinary(archivePath, tmpDir, innerName string) (string, error) {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractZipBinary(archivePath, tmpDir, innerName)
	}
	return extractBinary(archivePath, tmpDir, innerName)
}

func extractBinary(tarPath, tmpDir, innerName string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxExpandedSize+(1<<20)))
	extracted := ""
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		// The release format is exactly one root-level regular binary. Do not
		// accept a matching basename nested under an attacker-controlled path.
		if hdr.Name != innerName || extracted != "" {
			return "", fmt.Errorf("tarball contains unexpected entry %s", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return "", fmt.Errorf("archive entry %s is not a regular file", hdr.Name)
		}
		if hdr.Size < 0 || hdr.Size > maxExpandedSize {
			return "", fmt.Errorf("archive entry %s exceeds %d bytes", hdr.Name, maxExpandedSize)
		}
		out := filepath.Join(tmpDir, innerName)
		of, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return "", err
		}
		n, copyErr := io.Copy(of, io.LimitReader(tr, maxExpandedSize+1))
		if copyErr != nil {
			of.Close()
			_ = os.Remove(out)
			return "", copyErr
		}
		if n != hdr.Size || n > maxExpandedSize {
			of.Close()
			_ = os.Remove(out)
			return "", fmt.Errorf("archive entry %s has invalid extracted size", hdr.Name)
		}
		if err := of.Close(); err != nil {
			return "", err
		}
		extracted = out
	}
	if extracted == "" {
		return "", fmt.Errorf("tarball missing %s", innerName)
	}
	return extracted, nil
}

func extractZipBinary(zipPath, tmpDir, innerName string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	if len(zr.File) != 1 {
		return "", fmt.Errorf("zip must contain exactly one root entry named %s", innerName)
	}
	entry := zr.File[0]
	if entry.Name != innerName || entry.FileInfo().IsDir() || !entry.Mode().IsRegular() {
		return "", fmt.Errorf("zip entry must be one root-level regular file named %s", innerName)
	}
	if entry.UncompressedSize64 > uint64(maxExpandedSize) {
		return "", fmt.Errorf("zip entry %s exceeds %d bytes", innerName, maxExpandedSize)
	}
	r, err := entry.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	out := filepath.Join(tmpDir, innerName)
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(f, io.LimitReader(r, maxExpandedSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n != int64(entry.UncompressedSize64) || n > maxExpandedSize {
		_ = os.Remove(out)
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return "", fmt.Errorf("zip entry %s has invalid extracted size", innerName)
	}
	return out, nil
}

func verifyCandidate(ctx context.Context, binary, version string) error {
	reported, output, err := reportedBinaryVersion(ctx, binary)
	if err != nil {
		return fmt.Errorf("%s version failed: %w: %s", filepath.Base(binary), err, output)
	}
	want, err := normalizeVersion(version)
	if err != nil {
		return err
	}
	if reported != want {
		return fmt.Errorf("binary reported %q, expected version %s", output, want)
	}
	return nil
}

func reportedBinaryVersion(ctx context.Context, binary string) (version, output string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(vctx, binary, "version")
	var captured limitedBuffer
	captured.limit = maxVerifyOutput
	cmd.Stdout = &captured
	cmd.Stderr = &captured
	if err := cmd.Run(); err != nil {
		return "", strings.TrimSpace(captured.String()), err
	}
	trimmed := strings.TrimSpace(captured.String())
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", trimmed, fmt.Errorf("empty version output")
	}
	first := fields[0]
	if strings.HasPrefix(first, "v") {
		first = strings.TrimPrefix(first, "v")
	}
	if err := validateNormalizedVersion(first); err != nil {
		return "", trimmed, fmt.Errorf("invalid leading version field %q", fields[0])
	}
	return first, trimmed, nil
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func setAuth(req *http.Request, token, accept string) {
	if token = strings.TrimSpace(token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
}

// IsNewer reports whether want is strictly newer than have. Both are expected
// to be semver-ish like "0.1.0", "v0.1.1", or "0.2.0-rc1". Non-numeric
// segments compare lexicographically; that's enough for monotonic releases.
func IsNewer(have, want string) bool {
	have = strings.TrimPrefix(have, "v")
	want = strings.TrimPrefix(want, "v")
	if have == want {
		return false
	}
	hp := strings.SplitN(have, "-", 2)
	wp := strings.SplitN(want, "-", 2)
	hs := strings.Split(hp[0], ".")
	ws := strings.Split(wp[0], ".")
	for i := 0; i < len(hs) || i < len(ws); i++ {
		var a, b string
		if i < len(hs) {
			a = hs[i]
		}
		if i < len(ws) {
			b = ws[i]
		}
		ai, aok := atoi(a)
		bi, bok := atoi(b)
		if aok && bok {
			if ai != bi {
				return bi > ai
			}
			continue
		}
		if a != b {
			return b > a
		}
	}
	// Numeric prefix equal — pre-release suffix on `have` means `want` (no
	// suffix) is newer; suffix on `want` means it's older than a clean `have`.
	switch {
	case len(hp) == 2 && len(wp) == 1:
		return true
	case len(hp) == 1 && len(wp) == 2:
		return false
	case len(hp) == 2 && len(wp) == 2:
		return wp[1] > hp[1]
	}
	return false
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
