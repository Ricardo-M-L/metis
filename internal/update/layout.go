package update

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	retainedOldVersions = 2
	stagingMaxAge       = time.Hour
	lockOwnerFile       = "owner.json"
)

type installLayout struct {
	launcher         string
	managedRoot      string
	versionsRoot     string
	stagingRoot      string
	locksRoot        string
	runningLocksRoot string
	installLockDir   string
	currentVersion   string
}

func newInstallLayout(launcher, managedRoot string) installLayout {
	managedRoot = filepath.Clean(managedRoot)
	locksRoot := filepath.Join(managedRoot, "locks")
	return installLayout{
		launcher:         filepath.Clean(launcher),
		managedRoot:      managedRoot,
		versionsRoot:     filepath.Join(managedRoot, "versions"),
		stagingRoot:      filepath.Join(managedRoot, "staging"),
		locksRoot:        locksRoot,
		runningLocksRoot: filepath.Join(locksRoot, "running"),
		installLockDir:   filepath.Join(locksRoot, "install.lock.d"),
		currentVersion:   filepath.Join(managedRoot, "current-version"),
	}
}

func normalizeVersion(tag string) (string, error) {
	version := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if err := validateNormalizedVersion(version); err != nil {
		return "", fmt.Errorf("invalid release version %q", tag)
	}
	return version, nil
}

func validateNormalizedVersion(version string) error {
	if version == "" || version == "." || version == ".." || len(version) > 128 {
		return fmt.Errorf("invalid version")
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == '+' {
			continue
		}
		return fmt.Errorf("invalid version")
	}
	return nil
}

func versionBinary(layout installLayout, version string) string {
	return filepath.Join(layout.versionsRoot, version, executableName())
}

// versionFromBinaryPath accepts only a direct
// versions/<version>/<platform executable> path. It never evaluates symlinks.
func versionFromBinaryPath(layout installLayout, path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(layout.versionsRoot, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 2 || parts[1] != executableName() {
		return "", false
	}
	version, err := normalizeVersion(parts[0])
	if err != nil || version != parts[0] {
		return "", false
	}
	dirInfo, err := os.Lstat(filepath.Join(layout.versionsRoot, version))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	binInfo, err := os.Lstat(abs)
	if err != nil || !binInfo.Mode().IsRegular() || binInfo.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return version, true
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

func sameFileContents(a, b string) bool {
	ah, err := fileDigest(a)
	if err != nil {
		return false
	}
	bh, err := fileDigest(b)
	return err == nil && ah == bh
}

func fileDigest(path string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, err
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
