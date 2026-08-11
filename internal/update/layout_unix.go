//go:build !windows

package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func executableName() string { return "metis" }

func layoutForLauncher(launcher string) installLayout {
	launcher, _ = filepath.Abs(launcher)
	binDir := filepath.Dir(launcher)
	prefix := filepath.Dir(binDir)
	return newInstallLayout(launcher, filepath.Join(prefix, "share", "metis"))
}

func managedLauncherForExecutable(executable string) (string, bool) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return "", false
	}
	launcher, ok := launcherFromVersionExecutableShape(executable)
	if !ok {
		return "", false
	}
	layout := layoutForLauncher(launcher)
	version, ok := versionFromBinaryPath(layout, executable)
	if !ok {
		return "", false
	}
	if _, currentOK := resolveCurrentVersion(layout); !currentOK {
		return "", false
	}
	_ = version // executable itself was verified as a managed immutable version.
	return launcher, true
}

func launcherFromVersionExecutableShape(executable string) (string, bool) {
	if filepath.Base(executable) != executableName() {
		return "", false
	}
	versionDir := filepath.Dir(executable)
	if _, err := normalizeVersion(filepath.Base(versionDir)); err != nil {
		return "", false
	}
	versionsRoot := filepath.Dir(versionDir)
	managedRoot := filepath.Dir(versionsRoot)
	shareDir := filepath.Dir(managedRoot)
	if filepath.Base(versionsRoot) != "versions" || filepath.Base(managedRoot) != "metis" || filepath.Base(shareDir) != "share" {
		return "", false
	}
	prefix := filepath.Dir(shareDir)
	return filepath.Join(prefix, "bin", executableName()), true
}

func launcherFromBackupExecutableShape(string) (string, bool) {
	return "", false
}

func reconcileActivation(installLayout) error { return nil }

func resolveCurrentVersion(layout installLayout) (string, bool) {
	info, err := os.Lstat(layout.launcher)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(layout.launcher)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(layout.launcher), target)
	}
	target, err = filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", false
	}
	return versionFromBinaryPath(layout, target)
}

func activateVersion(layout installLayout, version, binary string) error {
	if err := os.MkdirAll(filepath.Dir(layout.launcher), 0o755); err != nil {
		return err
	}
	tmpLink := filepath.Join(filepath.Dir(layout.launcher), fmt.Sprintf(".metis-link-%d", os.Getpid()))
	_ = os.Remove(tmpLink)
	if err := os.Symlink(binary, tmpLink); err != nil {
		return fmt.Errorf("create temporary launcher symlink: %w", err)
	}
	if err := os.Rename(tmpLink, layout.launcher); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("activate launcher: %w", err)
	}
	return nil
}

func cleanupPlatformTemps(layout installLayout, cutoff int64) {
	dir := filepath.Dir(layout.launcher)
	dirInfo, dirErr := os.Lstat(dir)
	if dirErr != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".metis-link-") {
			continue
		}
		path := filepath.Join(filepath.Dir(layout.launcher), entry.Name())
		info, err := os.Lstat(path)
		if err == nil && info.ModTime().UnixNano() < cutoff {
			_ = os.Remove(path) // removes a link itself; never follows it
		}
	}
}
