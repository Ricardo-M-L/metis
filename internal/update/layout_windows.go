//go:build windows

package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func executableName() string { return "metis.exe" }

func layoutForLauncher(launcher string) installLayout {
	launcher, _ = filepath.Abs(launcher)
	// %LOCALAPPDATA%\Programs\Metis\bin\metis.exe -> ...\Metis.
	return newInstallLayout(launcher, filepath.Dir(filepath.Dir(launcher)))
}

func managedLauncherForExecutable(executable string) (string, bool) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return "", false
	}
	// The normal Windows process path is the stable visible launcher.
	if strings.EqualFold(filepath.Base(executable), executableName()) && strings.EqualFold(filepath.Base(filepath.Dir(executable)), "bin") {
		layout := layoutForLauncher(executable)
		if _, ok := resolveCurrentVersion(layout); ok {
			return executable, true
		}
	}
	// During activation Windows may report the renamed, still-running old
	// image. Accept only the exact backup shape and only when its contents match
	// an immutable managed version and the stable launcher is itself valid.
	if launcher, ok := launcherFromBackupExecutableShape(executable); ok {
		layout := layoutForLauncher(launcher)
		if _, currentOK := resolveCurrentVersion(layout); currentOK && matchesAnyManagedVersion(layout, executable) {
			return launcher, true
		}
		return "", false
	}
	// Also accept the immutable versions/<version>/metis.exe path, which is
	// useful for diagnostics and tests even though activation copies it.
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
	_ = version
	return launcher, true
}

func launcherFromBackupExecutableShape(executable string) (string, bool) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(filepath.Base(filepath.Dir(executable)), "bin") {
		return "", false
	}
	name := strings.ToLower(filepath.Base(executable))
	const prefix = ".metis.old."
	const suffix = ".exe"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return "", false
	}
	fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix), ".")
	if len(fields) != 2 || !validInstallLockNonce(fields[1]) {
		return "", false
	}
	stamp, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || stamp <= 0 {
		return "", false
	}
	return filepath.Join(filepath.Dir(executable), executableName()), true
}

func launcherFromVersionExecutableShape(executable string) (string, bool) {
	if !strings.EqualFold(filepath.Base(executable), executableName()) {
		return "", false
	}
	versionDir := filepath.Dir(executable)
	if _, err := normalizeVersion(filepath.Base(versionDir)); err != nil {
		return "", false
	}
	versionsRoot := filepath.Dir(versionDir)
	if !strings.EqualFold(filepath.Base(versionsRoot), "versions") {
		return "", false
	}
	managedRoot := filepath.Dir(versionsRoot)
	return filepath.Join(managedRoot, "bin", executableName()), true
}

func resolveCurrentVersion(layout installLayout) (string, bool) {
	b, err := os.ReadFile(layout.currentVersion)
	if err != nil {
		return "", false
	}
	version, err := normalizeVersion(strings.TrimSpace(string(b)))
	if err != nil {
		return "", false
	}
	versioned := versionBinary(layout, version)
	info, err := os.Lstat(versioned)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	launcherInfo, err := os.Lstat(layout.launcher)
	if err != nil || !launcherInfo.Mode().IsRegular() || launcherInfo.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	if !sameFileContents(layout.launcher, versioned) {
		return "", false
	}
	return version, true
}

func matchesAnyManagedVersion(layout installLayout, candidate string) bool {
	info, err := os.Lstat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	rootInfo, err := os.Lstat(layout.versionsRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	entries, err := os.ReadDir(layout.versionsRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		version, versionErr := normalizeVersion(entry.Name())
		if versionErr != nil || version != entry.Name() {
			continue
		}
		versioned := versionBinary(layout, version)
		if _, ok := versionFromBinaryPath(layout, versioned); ok && sameFileContents(candidate, versioned) {
			return true
		}
	}
	return false
}

// reconcileActivation repairs the crash window between switching the visible
// launcher and committing current-version. The marker is authoritative. A
// mismatched launcher is replaced only when it is provably another immutable
// managed version; arbitrary/custom executables are never overwritten.
func reconcileActivation(layout installLayout) error {
	b, err := os.ReadFile(layout.currentVersion)
	if errors.Is(err, os.ErrNotExist) {
		return nil // legacy/first install, handled by migration later
	}
	if err != nil {
		return err
	}
	version, err := normalizeVersion(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("invalid current-version marker")
	}
	markerBinary := versionBinary(layout, version)
	if _, ok := versionFromBinaryPath(layout, markerBinary); !ok {
		return fmt.Errorf("current-version %s has no valid immutable binary", version)
	}

	launcherInfo, launcherErr := os.Lstat(layout.launcher)
	if launcherErr == nil {
		if !launcherInfo.Mode().IsRegular() || launcherInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to reconcile non-regular launcher %s", layout.launcher)
		}
		if sameFileContents(layout.launcher, markerBinary) {
			return nil
		}
		if !matchesAnyManagedVersion(layout, layout.launcher) {
			return fmt.Errorf("launcher differs from current-version and is not a managed Metis binary")
		}
	} else if !errors.Is(launcherErr, os.ErrNotExist) {
		return launcherErr
	}

	if err := ensureDirectDirectory(filepath.Dir(layout.launcher), 0o755); err != nil {
		return err
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	staged := filepath.Join(filepath.Dir(layout.launcher), ".metis.reconcile."+nonce+".exe")
	if err := copyFile(markerBinary, staged, 0o755); err != nil {
		return fmt.Errorf("stage marker-authoritative launcher: %w", err)
	}
	defer os.Remove(staged)

	displaced := ""
	if launcherErr == nil {
		displaced = filepath.Join(filepath.Dir(layout.launcher), ".metis.recovery."+nonce+".exe")
		if err := os.Rename(layout.launcher, displaced); err != nil {
			return fmt.Errorf("preserve interrupted launcher: %w", err)
		}
	}
	if err := os.Rename(staged, layout.launcher); err != nil {
		if displaced != "" {
			_ = os.Rename(displaced, layout.launcher)
		}
		return fmt.Errorf("restore current-version launcher: %w", err)
	}
	if displaced != "" {
		_ = os.Remove(displaced) // a running image may remain locked; housekeeping retries
	}
	return nil
}

func activateVersion(layout installLayout, version, binary string) error {
	if err := os.MkdirAll(filepath.Dir(layout.launcher), 0o755); err != nil {
		return err
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	staged := filepath.Join(filepath.Dir(layout.launcher), ".metis.new."+nonce+".exe")
	backup := filepath.Join(filepath.Dir(layout.launcher), fmt.Sprintf(".metis.old.%d.%s.exe", time.Now().Unix(), nonce))
	if err := copyFile(binary, staged, 0o755); err != nil {
		return fmt.Errorf("stage Windows launcher: %w", err)
	}
	defer os.Remove(staged)

	hadLauncher := false
	if info, statErr := os.Lstat(layout.launcher); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to replace non-regular launcher %s", layout.launcher)
		}
		if err := os.Rename(layout.launcher, backup); err != nil {
			return fmt.Errorf("preserve running launcher: %w", err)
		}
		hadLauncher = true
	} else if !os.IsNotExist(statErr) {
		return statErr
	}

	rollback := func() {
		_ = os.Remove(layout.launcher)
		if hadLauncher {
			_ = os.Rename(backup, layout.launcher)
		}
	}
	if err := os.Rename(staged, layout.launcher); err != nil {
		rollback()
		return fmt.Errorf("activate Windows launcher: %w", err)
	}
	if err := writeFileAtomic(layout.currentVersion, []byte(version+"\n"), 0o644); err != nil {
		rollback()
		return fmt.Errorf("record current version: %w", err)
	}
	if hadLauncher {
		_ = os.Remove(backup) // a running .exe may remain locked; housekeeping retries
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
		name := entry.Name()
		if !strings.HasPrefix(name, ".metis.old.") && !strings.HasPrefix(name, ".metis.new.") &&
			!strings.HasPrefix(name, ".metis.reconcile.") && !strings.HasPrefix(name, ".metis.recovery.") {
			continue
		}
		path := filepath.Join(filepath.Dir(layout.launcher), name)
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.ModTime().UnixNano() < cutoff {
			_ = os.Remove(path)
		}
	}
}
