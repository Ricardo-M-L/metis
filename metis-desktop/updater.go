package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	desktopUpdateRepo       = "Ricardo-M-L/metis"
	desktopUpdateWebBase    = "https://github.com"
	desktopUpdateMaxArchive = int64(256 << 20)
	desktopUpdateMaxExpand  = int64(384 << 20)
)

// DesktopUpdateStatus is the deliberately small contract exposed to the
// embedded WebView. The renderer only needs to know whether to paint the
// update dot and which version will be installed; release internals and URLs
// used for downloading stay in the native process.
type DesktopUpdateStatus struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Available      bool   `json:"available"`
	CanUpdate      bool   `json:"canUpdate"`
	Installed      bool   `json:"installed,omitempty"`
	Restarting     bool   `json:"restarting,omitempty"`
	ReleaseURL     string `json:"releaseUrl,omitempty"`
	Message        string `json:"message,omitempty"`
}

type desktopUpdater struct {
	webBase  string
	repo     string
	client   *http.Client
	goos     string
	goarch   string
	validate func(path, version string) error
}

func defaultDesktopUpdater() desktopUpdater {
	u := desktopUpdater{
		webBase: desktopUpdateWebBase,
		repo:    desktopUpdateRepo,
		client:  &http.Client{Timeout: 10 * time.Minute},
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
	}
	u.validate = func(path, version string) error {
		return validateDesktopCandidate(path, version, u.goos)
	}
	return u
}

func desktopAssetName(goos, goarch string) (string, error) {
	switch goos {
	case "darwin":
		return "metis-desktop-darwin-universal.zip", nil
	case "linux":
		if goarch == "amd64" {
			return "metis-desktop-linux-amd64.tar.gz", nil
		}
	case "windows":
		if goarch == "amd64" {
			return "metis-desktop-windows-amd64.zip", nil
		}
	}
	return "", fmt.Errorf("in-app Desktop updates are not published for %s/%s", goos, goarch)
}

func (u desktopUpdater) Check(ctx context.Context, current string) (DesktopUpdateStatus, error) {
	status := DesktopUpdateStatus{CurrentVersion: normalizeDesktopVersion(current)}
	assetName, assetErr := desktopAssetName(u.goos, u.goarch)
	tag, releaseURL, err := u.latestTag(ctx)
	if err != nil {
		return status, err
	}
	status.LatestVersion = normalizeDesktopVersion(tag)
	status.ReleaseURL = releaseURL
	status.Available = desktopVersionNewer(status.CurrentVersion, status.LatestVersion)
	status.CanUpdate = assetErr == nil && assetName != ""
	if u.goos == "windows" && status.CanUpdate {
		// Replacing a running Windows executable requires a separately signed
		// hand-off helper. Do not advertise an update action that can download
		// successfully but cannot activate or restart safely.
		status.CanUpdate = false
		status.Message = "Automatic Desktop restart is not supported on Windows yet"
	} else if assetErr != nil {
		status.Message = assetErr.Error()
	} else if status.Available {
		status.Message = fmt.Sprintf("Metis Desktop %s is available", status.LatestVersion)
	} else {
		status.Message = fmt.Sprintf("Metis Desktop %s is up to date", status.CurrentVersion)
	}
	return status, nil
}

func (u desktopUpdater) Install(ctx context.Context, current, appPath string) (DesktopUpdateStatus, error) {
	status, err := u.Check(ctx, current)
	if err != nil {
		return status, err
	}
	if !status.Available {
		return status, errors.New("Metis Desktop is already up to date")
	}
	if !status.CanUpdate {
		return status, errors.New(status.Message)
	}
	assetName, _ := desktopAssetName(u.goos, u.goarch)
	tag := "v" + status.LatestVersion
	assetBase := strings.TrimRight(u.webBase, "/") + "/" + strings.Trim(u.repo, "/") + "/releases/download/" + url.PathEscape(tag) + "/"

	appPath, err = validateDesktopDestination(appPath, u.goos)
	if err != nil {
		return status, err
	}
	releaseLock, err := acquireDesktopUpdateLock(appPath)
	if err != nil {
		return status, err
	}
	defer func() { _ = releaseLock() }()
	parent := filepath.Dir(appPath)
	stageRoot, err := os.MkdirTemp(parent, ".metis-update-")
	if err != nil {
		return status, fmt.Errorf("create update staging directory beside Desktop app: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	if err := os.Chmod(stageRoot, 0o700); err != nil {
		return status, err
	}

	archivePath := filepath.Join(stageRoot, assetName)
	if err := u.download(ctx, assetBase+url.PathEscape(assetName), archivePath, desktopUpdateMaxArchive); err != nil {
		return status, fmt.Errorf("download Desktop update: %w", err)
	}
	wantSum, err := u.downloadChecksum(ctx, assetBase+url.PathEscape(assetName+".sha256"), assetName)
	if err != nil {
		return status, fmt.Errorf("download Desktop checksum: %w", err)
	}
	gotSum, err := desktopFileSHA256(archivePath)
	if err != nil {
		return status, err
	}
	if !strings.EqualFold(gotSum, wantSum) {
		return status, fmt.Errorf("Desktop update checksum mismatch: got %s want %s", gotSum, wantSum)
	}

	extractRoot := filepath.Join(stageRoot, "extract")
	if err := os.Mkdir(extractRoot, 0o700); err != nil {
		return status, err
	}
	candidate, err := extractDesktopArchive(archivePath, extractRoot, u.goos)
	if err != nil {
		return status, fmt.Errorf("extract Desktop update: %w", err)
	}
	validator := u.validate
	if validator == nil {
		validator = func(path, version string) error { return validateDesktopCandidate(path, version, u.goos) }
	}
	if err := validator(candidate, status.LatestVersion); err != nil {
		return status, fmt.Errorf("verify Desktop update: %w", err)
	}
	if err := activateDesktopCandidate(candidate, appPath, u.goos); err != nil {
		return status, err
	}

	status.CurrentVersion = status.LatestVersion
	status.Available = false
	status.Installed = true
	status.Message = fmt.Sprintf("Metis Desktop %s installed", status.LatestVersion)
	return status, nil
}

func (u desktopUpdater) latestTag(ctx context.Context) (tag, releaseURL string, err error) {
	latestURL := strings.TrimRight(u.webBase, "/") + "/" + strings.Trim(u.repo, "/") + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "metis-desktop-updater")
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("release check returned HTTP %d", resp.StatusCode)
	}
	prefix := "/" + strings.Trim(u.repo, "/") + "/releases/tag/"
	if !strings.HasPrefix(resp.Request.URL.Path, prefix) {
		return "", "", fmt.Errorf("latest release did not resolve to a version tag")
	}
	escaped := strings.TrimPrefix(resp.Request.URL.Path, prefix)
	tag, err = url.PathUnescape(escaped)
	if err != nil || strings.Contains(tag, "/") || !validDesktopVersion(tag) {
		return "", "", fmt.Errorf("invalid latest Desktop version %q", escaped)
	}
	return tag, resp.Request.URL.String(), nil
}

func (u desktopUpdater) httpClient() *http.Client {
	if u.client != nil {
		return u.client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (u desktopUpdater) download(ctx context.Context, rawURL, path string, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "metis-desktop-updater")
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return fmt.Errorf("asset is too large: %d bytes", resp.ContentLength)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, max+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	if n > max {
		_ = os.Remove(path)
		return fmt.Errorf("asset exceeds %d bytes", max)
	}
	return nil
}

func (u desktopUpdater) downloadChecksum(ctx context.Context, rawURL, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "metis-desktop-updater")
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || filepath.Base(strings.TrimPrefix(parts[len(parts)-1], "*")) != assetName {
			continue
		}
		if len(parts[0]) != sha256.Size*2 {
			return "", errors.New("checksum is not SHA-256")
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return "", errors.New("checksum is not valid hexadecimal SHA-256")
		}
		return strings.ToLower(parts[0]), nil
	}
	return "", fmt.Errorf("checksum file has no entry for %s", assetName)
}

func desktopFileSHA256(path string) (string, error) {
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

func extractDesktopArchive(archivePath, dest, goos string) (string, error) {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		if err := extractDesktopZip(archivePath, dest); err != nil {
			return "", err
		}
	} else {
		if err := extractDesktopTar(archivePath, dest); err != nil {
			return "", err
		}
	}
	switch goos {
	case "darwin":
		return filepath.Join(dest, "metis-desktop.app"), nil
	case "windows":
		return filepath.Join(dest, "metis-desktop.exe"), nil
	default:
		return filepath.Join(dest, "metis-desktop"), nil
	}
}

func extractDesktopZip(zipPath, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	var expanded int64
	for _, entry := range zr.File {
		name, err := safeArchivePath(entry.Name)
		if err != nil {
			return err
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe symbolic link in archive: %s", entry.Name)
		}
		if entry.UncompressedSize64 > uint64(desktopUpdateMaxExpand) {
			return fmt.Errorf("archive entry is too large: %s", entry.Name)
		}
		expanded += int64(entry.UncompressedSize64)
		if expanded > desktopUpdateMaxExpand {
			return fmt.Errorf("Desktop archive expands beyond %d bytes", desktopUpdateMaxExpand)
		}
		target := filepath.Join(dest, name)
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("unsafe non-regular archive entry: %s", entry.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		r, err := entry.Open()
		if err != nil {
			return err
		}
		mode := entry.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			r.Close()
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(r, int64(entry.UncompressedSize64)+1))
		closeErr := f.Close()
		r.Close()
		if copyErr != nil || closeErr != nil || n != int64(entry.UncompressedSize64) {
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			return fmt.Errorf("archive entry size changed while extracting: %s", entry.Name)
		}
	}
	return nil
}

func extractDesktopTar(tarPath, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, desktopUpdateMaxExpand+(1<<20)))
	seen := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name, err := safeArchivePath(hdr.Name)
		if err != nil {
			return err
		}
		if seen || name != "metis-desktop" || (hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA) {
			return fmt.Errorf("Desktop tarball must contain one regular metis-desktop binary")
		}
		if hdr.Size < 0 || hdr.Size > desktopUpdateMaxExpand {
			return fmt.Errorf("Desktop executable is too large")
		}
		out := filepath.Join(dest, name)
		of, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(of, io.LimitReader(tr, hdr.Size+1))
		closeErr := of.Close()
		if copyErr != nil || closeErr != nil || n != hdr.Size {
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			return errors.New("Desktop executable size changed while extracting")
		}
		seen = true
	}
	if !seen {
		return errors.New("Desktop tarball is empty")
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	name = filepath.Clean(filepath.FromSlash(name))
	if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe archive path: %s", name)
	}
	return name, nil
}

func validateDesktopDestination(path, goos string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil || abs == "" || abs == string(os.PathSeparator) {
		return "", errors.New("invalid Desktop application path")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("locate Desktop application: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("refusing to update a symlinked Desktop application")
	}
	if goos == "darwin" {
		if !info.IsDir() || !strings.HasSuffix(strings.ToLower(abs), ".app") {
			return "", errors.New("macOS Desktop path is not an application bundle")
		}
	} else if !info.Mode().IsRegular() {
		return "", errors.New("Desktop path is not a regular executable")
	}
	return abs, nil
}

func validateDesktopCandidate(path, version, goos string) error {
	switch goos {
	case "darwin":
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("downloaded macOS application bundle is invalid")
		}
		executable := filepath.Join(path, "Contents", "MacOS", "metis-desktop")
		if info, err := os.Lstat(executable); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return errors.New("downloaded macOS bundle has no executable")
		}
		plist, err := os.ReadFile(filepath.Join(path, "Contents", "Info.plist"))
		if err != nil || !strings.Contains(string(plist), version) {
			return fmt.Errorf("downloaded macOS bundle does not report version %s", version)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "codesign", "--verify", "--deep", "--strict", path).CombinedOutput(); err != nil {
			return fmt.Errorf("code signature verification failed: %s", strings.TrimSpace(string(output)))
		}
		return nil
	case "windows":
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(path), ".exe") {
			return errors.New("downloaded Windows executable is invalid")
		}
		return nil
	default:
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return errors.New("downloaded Linux executable is invalid")
		}
		return nil
	}
}

func activateDesktopCandidate(candidate, appPath, goos string) error {
	backup := appPath + ".previous"
	if filepath.Dir(backup) != filepath.Dir(appPath) || filepath.Base(backup) == "." {
		return errors.New("unsafe Desktop rollback path")
	}
	if err := removeDesktopRollback(backup, goos); err != nil {
		return fmt.Errorf("remove previous Desktop rollback: %w", err)
	}
	if err := os.Rename(appPath, backup); err != nil {
		return fmt.Errorf("preserve current Desktop version: %w", err)
	}
	if err := os.Rename(candidate, appPath); err != nil {
		rollbackErr := os.Rename(backup, appPath)
		if rollbackErr != nil {
			return fmt.Errorf("activate Desktop update: %v; rollback also failed: %v", err, rollbackErr)
		}
		return fmt.Errorf("activate Desktop update: %w", err)
	}
	return nil
}

func acquireDesktopUpdateLock(appPath string) (func() error, error) {
	lockPath := filepath.Join(filepath.Dir(appPath), "."+filepath.Base(appPath)+".metis-update.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := os.Lstat(lockPath)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("unsafe Desktop update lock already exists: %s", lockPath)
			}
			return nil, fmt.Errorf("another Desktop update is already in progress (lock: %s)", lockPath)
		}
		return nil, fmt.Errorf("create Desktop update lock: %w", err)
	}
	owned, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("inspect Desktop update lock: %w", err)
	}
	cleanupCreatedLock := func() {
		_ = lock.Close()
		if current, statErr := os.Lstat(lockPath); statErr == nil &&
			current.Mode()&os.ModeSymlink == 0 && current.Mode().IsRegular() && os.SameFile(owned, current) {
			_ = os.Remove(lockPath)
		}
	}
	if _, err := fmt.Fprintf(lock, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		cleanupCreatedLock()
		return nil, fmt.Errorf("write Desktop update lock: %w", err)
	}
	if err := lock.Sync(); err != nil {
		cleanupCreatedLock()
		return nil, fmt.Errorf("sync Desktop update lock: %w", err)
	}
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		closeErr := lock.Close()
		current, statErr := os.Lstat(lockPath)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			return closeErr
		case statErr != nil:
			return statErr
		case current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(owned, current):
			return fmt.Errorf("refusing to remove replaced Desktop update lock: %s", lockPath)
		}
		removeErr := os.Remove(lockPath)
		if closeErr != nil {
			return closeErr
		}
		return removeErr
	}, nil
}

func removeDesktopRollback(backup, goos string) error {
	info, err := os.Lstat(backup)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove a symlinked Desktop rollback")
	}
	if goos != "darwin" {
		if !info.Mode().IsRegular() {
			return errors.New("Desktop rollback is not a regular executable")
		}
		if goos != "windows" && info.Mode()&0o111 == 0 {
			return errors.New("Desktop rollback is not executable")
		}
		if err := validateDesktopRollbackBinary(backup, goos); err != nil {
			return err
		}
		return os.Remove(backup)
	}
	if !info.IsDir() {
		return errors.New("macOS Desktop rollback is not an application bundle")
	}
	if err := validateDesktopRollbackBundle(backup); err != nil {
		return err
	}
	return os.RemoveAll(backup)
}

func validateDesktopRollbackBinary(path, goos string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var header [4]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return errors.New("Desktop rollback executable is truncated")
	}
	switch goos {
	case "linux":
		if string(header[:]) != "\x7fELF" {
			return errors.New("Desktop rollback is not a Linux ELF executable")
		}
	case "windows":
		if string(header[:2]) != "MZ" {
			return errors.New("Desktop rollback is not a Windows PE executable")
		}
	default:
		return fmt.Errorf("unsupported Desktop rollback platform: %s", goos)
	}
	return nil
}

func validateDesktopRollbackBundle(path string) error {
	for _, dir := range []string{
		filepath.Join(path, "Contents"),
		filepath.Join(path, "Contents", "MacOS"),
	} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("macOS Desktop rollback has an unsafe bundle layout")
		}
	}
	executable := filepath.Join(path, "Contents", "MacOS", "metis-desktop")
	execInfo, err := os.Lstat(executable)
	if err != nil || !execInfo.Mode().IsRegular() || execInfo.Mode()&os.ModeSymlink != 0 || execInfo.Mode()&0o111 == 0 {
		return errors.New("macOS Desktop rollback has no direct executable")
	}
	plistPath := filepath.Join(path, "Contents", "Info.plist")
	plistInfo, err := os.Lstat(plistPath)
	if err != nil || !plistInfo.Mode().IsRegular() || plistInfo.Mode()&os.ModeSymlink != 0 || plistInfo.Size() > 1<<20 {
		return errors.New("macOS Desktop rollback has no safe Info.plist")
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil || !strings.Contains(string(plist), "com.metis.desktop") {
		return errors.New("macOS Desktop rollback does not identify METIS Desktop")
	}
	return nil
}

func currentDesktopPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "darwin" {
		return executable, nil
	}
	for path := filepath.Dir(executable); path != filepath.Dir(path); path = filepath.Dir(path) {
		if strings.HasSuffix(strings.ToLower(filepath.Base(path)), ".app") {
			return path, nil
		}
	}
	return "", errors.New("Desktop executable is not inside a macOS application bundle")
}

func restartDesktopProcess(appPath, workspace, metisBin string) error {
	cmd, err := restartDesktopCommand(runtime.GOOS, os.Getpid(), appPath, workspace, metisBin, "/usr/bin/open")
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func restartDesktopCommand(goos string, parentPID int, appPath, workspace, metisBin, opener string) (*exec.Cmd, error) {
	args := []string{"--workspace", workspace}
	if strings.TrimSpace(metisBin) != "" {
		args = append(args, "--metis-bin", metisBin)
	}
	switch goos {
	case "darwin":
		if parentPID <= 0 {
			return nil, errors.New("Desktop restart parent process is invalid")
		}
		if strings.TrimSpace(opener) == "" {
			opener = "/usr/bin/open"
		}
		// LaunchServices can absorb an open request into the still-running old
		// bundle instance. Keep the handoff in a detached helper and wait until
		// the old Wails process is gone before asking macOS to start the updated
		// bundle. Values are positional parameters rather than shell text so
		// paths containing spaces or shell metacharacters remain data.
		const script = `
parent_pid=$1
app_path=$2
workspace=$3
metis_bin=$4
opener=$5
while kill -0 "$parent_pid" 2>/dev/null; do
  /bin/sleep 0.1
done
if [ -n "$metis_bin" ]; then
  exec "$opener" -n -a "$app_path" --args --workspace "$workspace" --metis-bin "$metis_bin"
fi
exec "$opener" -n -a "$app_path" --args --workspace "$workspace"
`
		return exec.Command(
			"/bin/sh", "-c", script, "metis-desktop-restart",
			strconv.Itoa(parentPID), appPath, workspace, metisBin, opener,
		), nil
	case "linux":
		return exec.Command(appPath, args...), nil
	default:
		return nil, errors.New("automatic Desktop restart is not supported on this platform yet")
	}
}

func normalizeDesktopVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func validDesktopVersion(value string) bool {
	parts := strings.Split(normalizeDesktopVersion(value), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func desktopVersionNewer(current, latest string) bool {
	if !validDesktopVersion(current) || !validDesktopVersion(latest) {
		return false
	}
	a := strings.Split(normalizeDesktopVersion(current), ".")
	b := strings.Split(normalizeDesktopVersion(latest), ".")
	for i := 0; i < 3; i++ {
		ai, _ := strconv.Atoi(a[i])
		bi, _ := strconv.Atoi(b[i])
		if bi != ai {
			return bi > ai
		}
	}
	return false
}
