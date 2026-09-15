package computeruse

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"runtime"
	"strings"
)

//go:embed releases.json
var pinnedReleaseJSON []byte

// PinnedManifest returns a fresh copy of the catalog compiled into METIS. Only
// published, reviewed artifacts with explicit SHA256 digests belong here.
func PinnedManifest() Manifest {
	var manifest Manifest
	if err := json.Unmarshal(pinnedReleaseJSON, &manifest); err != nil {
		panic("invalid embedded computer-use release catalog: " + err.Error())
	}
	return manifest
}

func (m *Manager) release() (Release, error) {
	if len(m.manifest.Releases) == 0 {
		return Release{}, ErrNoOfficialRelease
	}
	if runtime.GOOS != "darwin" {
		return Release{}, errors.New("automatic computer-use installation is supported only on macOS; explicit local installs on this platform are experimental")
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	for _, release := range m.manifest.Releases {
		if release.Target != target || release.ProtocolVersion != ProtocolVersion {
			continue
		}
		if err := validateRelease(release); err != nil {
			return Release{}, err
		}
		return release, nil
	}
	return Release{}, fmt.Errorf("%w for %s (protocol %d)", ErrNoOfficialRelease, target, ProtocolVersion)
}

func validateRelease(release Release) error {
	if release.Version == "" || release.ProtocolVersion != ProtocolVersion || release.Target == "" {
		return errors.New("invalid pinned release version, target, or protocol")
	}
	if !validDigest(release.SHA256) {
		return errors.New("pinned release requires an explicit SHA256 digest")
	}
	if !validDigest(release.BinarySHA256) {
		return errors.New("pinned release requires an explicit binarySha256 executable digest")
	}
	u, err := url.Parse(release.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("pinned release requires an HTTPS URL without credentials or fragment")
	}
	if release.Size < 0 || release.Size > maxArtifactBytes {
		return errors.New("pinned release size exceeds installation limit")
	}
	if release.Format != "tar.gz" && release.Format != "zip" && release.Format != "binary" {
		return fmt.Errorf("unsupported pinned release format %q", release.Format)
	}
	if !safeRelative(release.BinaryPath) {
		return errors.New("invalid pinned release binaryPath")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func safeRelative(value string) bool {
	if value == "" || value == "." || strings.ContainsAny(value, "\\:\x00") || strings.HasPrefix(value, "/") {
		return false
	}
	return path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}
