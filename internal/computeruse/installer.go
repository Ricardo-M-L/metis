package computeruse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxArtifactBytes int64 = 128 << 20

type activation struct {
	Directory  string `json:"directory"`
	BinaryPath string `json:"binaryPath"`
	SHA256     string `json:"sha256"`
	Source     string `json:"source"`
	Version    string `json:"version"`
}

// Status verifies an explicitly selected local copy or this METIS build's
// pinned official version. A global official activation from a different METIS
// build cannot change which official version this build resolves.
// Enabled and Running are populated by the runtime controller.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	directory, err := m.directory()
	if err != nil {
		return Status{}, err
	}
	if err := checkManagedDirectories(directory, false); err != nil {
		return Status{}, err
	}
	activePath := filepath.Join(directory, "active.json")
	data, err := readRegularBounded(activePath, maxDescriptionBytes)
	activeExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, fmt.Errorf("read computer-use activation: %w", err)
	}
	var active activation
	if activeExists {
		if err := json.Unmarshal(data, &active); err != nil {
			return Status{}, fmt.Errorf("invalid computer-use activation: %w", err)
		}
		if err := validateActivation(active); err != nil {
			return Status{}, err
		}
		if active.Source == "local" {
			return inspectInstallation(ctx, directory, active, m.root)
		}
	}
	// Official metadata in active.json is only a source selector. Hashes,
	// versions, and paths come exclusively from this build's trusted catalog.
	release, err := m.release()
	if err != nil {
		if activeExists {
			return Status{Source: "official", Message: err.Error()}, err
		}
		return Status{Message: ErrNotInstalled.Error()}, nil
	}
	status, err := inspectInstallation(ctx, directory, officialActivation(release), m.root)
	if errors.Is(err, os.ErrNotExist) {
		return Status{Version: release.Version, Source: "official", Message: "This METIS build's pinned computer-use helper is not installed."}, nil
	}
	return status, err
}

func officialActivation(release Release) activation {
	return activation{Directory: "official-" + release.SHA256, BinaryPath: release.BinaryPath, SHA256: release.BinarySHA256, Source: "official", Version: release.Version}
}

func inspectInstallation(ctx context.Context, directory string, active activation, metisHome string) (Status, error) {
	versionDirectory := filepath.Join(directory, "versions", active.Directory)
	filename := filepath.Join(versionDirectory, filepath.FromSlash(active.BinaryPath))
	status := Status{Path: filename, Version: active.Version, Source: active.Source}
	if err := checkPathDirectories(versionDirectory, filepath.Dir(filename)); err != nil {
		return status, err
	}
	if err := checkExecutable(filename); err != nil {
		status.Message = err.Error()
		return status, err
	}
	digest, err := fileDigest(ctx, filename)
	if err != nil {
		return status, err
	}
	if digest != active.SHA256 {
		return status, errors.New("installed computer-use helper checksum mismatch against selected source")
	}
	description, err := probeWithHome(ctx, filename, metisHome)
	if err != nil {
		status.Message = err.Error()
		return status, err
	}
	if description.Version != active.Version {
		return status, errors.New("installed computer-use helper version differs from selected source")
	}
	status.Installed = true
	status.Description = &description
	status.Message = "Computer-use helper installed; permissions and process state are checked separately."
	if active.Source == "local" {
		status.Message = "Explicit local build installed; this is not a verified official release."
		if runtime.GOOS != "darwin" {
			status.Message += " Computer use on this platform is experimental and unverified."
		}
	}
	return status, nil
}

func validateActivation(active activation) error {
	if active.Source != "local" && active.Source != "official" {
		return errors.New("invalid computer-use activation source")
	}
	prefix := active.Source + "-"
	if !strings.HasPrefix(active.Directory, prefix) || !validDigest(strings.TrimPrefix(active.Directory, prefix)) || !validDigest(active.SHA256) {
		return errors.New("invalid computer-use activation directory or digest")
	}
	if !safeRelative(active.BinaryPath) || active.Version == "" {
		return errors.New("invalid computer-use activation binaryPath or version")
	}
	return nil
}

// Ensure installs only a matching release pinned in this METIS build. It does
// not discover unsigned manifests, execute PATH binaries, or elevate privileges.
func (m *Manager) Ensure(ctx context.Context) (Status, error) {
	status, err := m.Status(ctx)
	if err != nil || status.Installed {
		return status, err
	}
	release, err := m.release()
	if err != nil {
		status.Message = err.Error()
		return status, err
	}
	unlock, directory, err := m.lock(ctx)
	if err != nil {
		return status, err
	}
	defer unlock()
	status, err = m.Status(ctx)
	if err != nil || status.Installed {
		return status, err
	}
	stage, err := os.MkdirTemp(directory, ".stage-")
	if err != nil {
		return status, err
	}
	defer os.RemoveAll(stage)
	artifact := filepath.Join(stage, "download")
	if err := m.stageArtifact(ctx, release, artifact); err != nil {
		return status, err
	}
	payload := filepath.Join(stage, "payload")
	if err := os.Mkdir(payload, 0700); err != nil {
		return status, err
	}
	if err := extractArtifact(ctx, artifact, payload, release); err != nil {
		return status, err
	}
	filename := filepath.Join(payload, filepath.FromSlash(release.BinaryPath))
	if err := checkExecutable(filename); err != nil {
		return status, err
	}
	digest, err := fileDigest(ctx, filename)
	if err != nil {
		return status, err
	}
	if digest != release.BinarySHA256 {
		return status, errors.New("computer-use executable SHA256 mismatch against pinned binarySha256; installation aborted before execution")
	}
	description, err := probeWithHome(ctx, filename, m.root)
	if err != nil {
		return status, err
	}
	if description.Version != release.Version {
		return status, fmt.Errorf("release helper version %q does not match pinned version %q", description.Version, release.Version)
	}
	active := officialActivation(release)
	if err := activate(ctx, directory, payload, active); err != nil {
		return status, err
	}
	return m.Status(ctx)
}

// InstallLocal copies only the explicitly selected regular executable. It
// probes the staged copy and publishes it without overwriting another version.
func (m *Manager) InstallLocal(ctx context.Context, filename string) (Status, error) {
	if filename == "" {
		return Status{}, errors.New("explicit local helper path is required")
	}
	filename, err := filepath.Abs(filename)
	if err != nil {
		return Status{}, err
	}
	if err := checkExecutable(filename); err != nil {
		return Status{}, err
	}
	unlock, directory, err := m.lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	stage, err := os.MkdirTemp(directory, ".stage-")
	if err != nil {
		return Status{}, err
	}
	defer os.RemoveAll(stage)
	binaryName := "metis-cu"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	destination := filepath.Join(stage, binaryName)
	if err := copyRegular(ctx, filename, destination, 0500, maxArtifactBytes); err != nil {
		return Status{}, err
	}
	digest, err := fileDigest(ctx, destination)
	if err != nil {
		return Status{}, err
	}
	description, err := probeWithHome(ctx, destination, m.root)
	if err != nil {
		return Status{}, err
	}
	active := activation{Directory: "local-" + digest, BinaryPath: binaryName, SHA256: digest, Source: "local", Version: description.Version}
	if err := activate(ctx, directory, stage, active); err != nil {
		return Status{}, err
	}
	return m.Status(ctx)
}

func activate(ctx context.Context, directory, stage string, active activation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateActivation(active); err != nil {
		return err
	}
	destination := filepath.Join(directory, "versions", active.Directory)
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("installed version path is not a regular directory")
		}
		filename := filepath.Join(destination, filepath.FromSlash(active.BinaryPath))
		if err := checkPathDirectories(destination, filepath.Dir(filename)); err != nil {
			return err
		}
		if err := checkExecutable(filename); err != nil {
			return err
		}
		digest, err := fileDigest(ctx, filename)
		if err != nil {
			return err
		}
		if digest != active.SHA256 {
			return errors.New("existing immutable computer-use version has a different checksum")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := os.Rename(stage, destination); err != nil {
		return fmt.Errorf("publish computer-use version: %w", err)
	}
	data, err := json.Marshal(active)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".active-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(directory, "active.json")); err != nil {
		return fmt.Errorf("activate computer-use version: %w", err)
	}
	return nil
}

func checkManagedDirectories(directory string, create bool) error {
	// The user chooses METIS_HOME; reject symlink substitution within the managed
	// subtree, without rejecting normal OS aliases in ancestors such as /var.
	root := filepath.Dir(filepath.Dir(directory))
	if create {
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
	}
	for _, name := range []string{filepath.Dir(directory), directory, filepath.Join(directory, "versions")} {
		if create {
			if err := os.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		}
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && !create {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed computer-use path is not a regular directory: %s", name)
		}
		if info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("managed computer-use directory must not be writable by group or others: %s", name)
		}
	}
	return nil
}

func checkPathDirectories(root, directory string) error {
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("computer-use path escapes version directory")
	}
	current := root
	parts := []string{"."}
	if relative != "." {
		parts = append(parts, strings.Split(relative, string(filepath.Separator))...)
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("computer-use binary path contains a symlink or non-directory")
		}
	}
	return nil
}

func (m *Manager) download(ctx context.Context, release Release, destination string) error {
	if err := validateRelease(release); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return err
	}
	client := *m.client
	priorRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "https" || request.URL.User != nil {
			return errors.New("release redirect must use HTTPS without credentials")
		}
		if len(via) >= 10 {
			return errors.New("too many release redirects")
		}
		if priorRedirect != nil {
			return priorRedirect(request, via)
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download pinned computer-use release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("computer-use release download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxArtifactBytes {
		return errors.New("computer-use release exceeds download size limit")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxArtifactBytes {
		return errors.New("computer-use release exceeds download size limit")
	}
	if release.Size > 0 && n != release.Size {
		return errors.New("computer-use release size differs from pinned size")
	}
	if hex.EncodeToString(hash.Sum(nil)) != release.SHA256 {
		return errors.New("computer-use release SHA256 mismatch; installation aborted")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func openRegular(filename string) (*os.File, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("computer-use file must be regular and not a symlink")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		file.Close()
		return nil, errors.New("computer-use file changed while opening")
	}
	return file, nil
}

func readRegularBounded(filename string, limit int64) ([]byte, error) {
	file, err := openRegular(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("computer-use metadata exceeds size limit")
	}
	return data, nil
}

func fileDigest(ctx context.Context, filename string) (string, error) {
	file, err := openRegular(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, file}, maxArtifactBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxArtifactBytes {
		return "", errors.New("computer-use binary exceeds size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyRegular(ctx context.Context, source, destination string, mode os.FileMode, limit int64) error {
	input, err := openRegular(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(output, io.LimitReader(contextReader{ctx, input}, limit+1))
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if n > limit {
		return errors.New("computer-use file exceeds size limit")
	}
	return closeErr
}
