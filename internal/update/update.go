// Package update implements self-update for Metis against a private GitHub
// release. All network calls require a GitHub PAT with read access to the
// repo (METIS_GITHUB_TOKEN, falling back to GITHUB_TOKEN).
package update

import (
	"archive/tar"
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
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

const (
	defaultRepo  = "Ricardo-M-L/metis"
	apiBase      = "https://api.github.com"
	userAgent    = "metis-self-update"
	checkTimeout = 10 * time.Second
	dlTimeout    = 5 * time.Minute
)

// Repo identifies the GitHub owner/repo to query. Override via METIS_REPO.
func Repo() string {
	if v := strings.TrimSpace(os.Getenv("METIS_REPO")); v != "" {
		return v
	}
	return defaultRepo
}

// Token returns the PAT to authenticate against GitHub, or "" if none set.
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
// Without #3, the version-check + self-update silently no-op'd on
// every machine where the user hadn't manually exported a PAT —
// "current: vX" showed but "latest: vY" never lit up because
// MaybeCheck bailed at the empty-token guard. (User report
// 2026-05-10: "metis 打开会不会显示当前版本和最新版本" — answer was
// "yes in code, no in practice because no token".)
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

// ErrNoToken is returned when no PAT is configured.
var ErrNoToken = errors.New("no GitHub token set (METIS_GITHUB_TOKEN or GITHUB_TOKEN)")

type asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
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
	if token == "" {
		return nil, ErrNoToken
	}
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

// Target returns the platform tag (e.g. "darwin-arm64") used in asset names.
func Target() string {
	return fmt.Sprintf("%s-%s", goruntime.GOOS, goruntime.GOARCH)
}

// findAsset returns the asset whose name matches `metis-<target>.tar.gz`
// and its sha256 sibling.
func (r *release) findAsset(target string) (binary, sum *asset, err error) {
	wantBin := fmt.Sprintf("metis-%s.tar.gz", target)
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

// Apply downloads the latest release for the current platform, verifies its
// sha256, and atomically replaces the file at destPath. destPath should be
// the absolute path to the running metis binary.
func Apply(ctx context.Context, token, destPath string, r *release) error {
	if token == "" {
		return ErrNoToken
	}
	if r == nil {
		var err error
		r, err = Latest(ctx, token)
		if err != nil {
			return err
		}
	}
	target := Target()
	binAsset, sumAsset, err := r.findAsset(target)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "metis-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tarPath := filepath.Join(tmpDir, binAsset.Name)
	if err := downloadAsset(ctx, token, binAsset.ID, tarPath); err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}

	wantSum, err := fetchSum(ctx, token, sumAsset.ID, binAsset.Name)
	if err != nil {
		return fmt.Errorf("fetch sha256: %w", err)
	}
	gotSum, err := sha256File(tarPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(gotSum, wantSum) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", gotSum, wantSum)
	}

	innerName := fmt.Sprintf("metis-%s", target)
	binPath, err := extractBinary(tarPath, tmpDir, innerName)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return err
	}
	return atomicReplace(binPath, destPath)
}

func downloadAsset(ctx context.Context, token string, id int64, dest string) error {
	url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", apiBase, Repo(), id)
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
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

// fetchSum reads the .sha256 sidecar file and returns the hex digest for the
// given filename. shasum format: "<hex>  <name>\n".
func fetchSum(ctx context.Context, token string, id int64, wantName string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", apiBase, Repo(), id)
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
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		// Reject anything that would write outside tmpDir.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return "", fmt.Errorf("tarball contains unsafe path: %s", hdr.Name)
		}
		if filepath.Base(clean) != innerName {
			continue
		}
		out := filepath.Join(tmpDir, innerName)
		of, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(of, tr); err != nil {
			of.Close()
			return "", err
		}
		if err := of.Close(); err != nil {
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("tarball missing %s", innerName)
}

// atomicReplace moves src over dest. On Unix os.Rename is atomic when both
// paths live on the same filesystem; we fall back to copy+rename otherwise.
func atomicReplace(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	// Cross-device fallback: copy to a sibling of dest, then rename.
	destDir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(destDir, ".metis-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	in, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return err
	}
	defer in.Close()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, dest)
}

func setAuth(req *http.Request, token, accept string) {
	req.Header.Set("Authorization", "Bearer "+token)
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
