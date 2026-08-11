//go:build windows

package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWindowsActivateAndResolveCurrent(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "bin", "metis.exe")
	layout := layoutForLauncher(launcher)
	oldBin := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Unix(1, 0))
	newBin := seedManagedVersion(t, layout.versionsRoot, "2.0.0", time.Unix(2, 0))
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(oldBin, launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(layout.currentVersion, []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := resolveCurrentVersion(layout); !ok || got != "1.0.0" {
		t.Fatalf("initial current = %q,%v", got, ok)
	}

	if err := activateVersion(layout, "2.0.0", newBin); err != nil {
		t.Fatalf("activateVersion: %v", err)
	}
	got, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(newBin)
	if !bytes.Equal(got, want) {
		t.Fatal("visible Windows launcher does not match activated immutable binary")
	}
	if current, ok := resolveCurrentVersion(layout); !ok || current != "2.0.0" {
		t.Fatalf("activated current = %q,%v", current, ok)
	}
	if _, err := os.Stat(oldBin); err != nil {
		t.Fatalf("old rollback version removed during activation: %v", err)
	}
}

func TestWindowsCleanupProtectsOldRunningVersion(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "bin", "metis.exe")
	layout := layoutForLauncher(launcher)
	paths := make(map[string]string)
	for i, version := range []string{"1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5"} {
		paths[version] = seedManagedVersion(t, layout.versionsRoot, version, time.Unix(int64(i+1), 0))
	}
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(paths["1.0.5"], launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(layout.currentVersion, []byte("1.0.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := registerRunningVersion(launcher, "1.0.1", paths["1.0.1"])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.1", "1.0.3", "1.0.4", "1.0.5"} {
		if _, err := os.Stat(paths[version]); err != nil {
			t.Errorf("protected/retained %s removed: %v", version, err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(paths["1.0.2"])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old unprotected 1.0.2 remains: %v", err)
	}
}

func TestWindowsOldBackupResolvesLauncherAndRegistersRunningVersion(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "bin", "metis.exe")
	layout := layoutForLauncher(launcher)
	oldBin := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Unix(1, 0))
	newBin := seedManagedVersion(t, layout.versionsRoot, "2.0.0", time.Unix(2, 0))
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(newBin, launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(layout.currentVersion, []byte("2.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(launcher), ".metis.old.1700000000.0123456789abcdef.exe")
	if err := copyFile(oldBin, backup, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSelfPath(backup, backup, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != launcher {
		t.Fatalf("resolveSelfPath = %q, want %q", got, launcher)
	}
	release, err := registerRunningVersion(launcher, "1.0.0", backup)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(layout.runningLocksRoot, "1.0.0", strconv.Itoa(os.Getpid())+".json")
	if _, err := os.Stat(lockPath); err != nil {
		release()
		t.Fatalf("old running version was not registered: %v", err)
	}
	release()
}

func TestWindowsReconcileInterruptedActivationUsesMarkerWithoutOverwritingCustom(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "bin", "metis.exe")
	layout := layoutForLauncher(launcher)
	oldBin := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Unix(1, 0))
	newBin := seedManagedVersion(t, layout.versionsRoot, "2.0.0", time.Unix(2, 0))
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(newBin, launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(layout.currentVersion, []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(launcher), ".metis.old.1700000000.0123456789abcdef.exe")
	if err := copyFile(oldBin, backup, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := reconcileActivation(layout); err != nil {
		t.Fatalf("reconcile crash state: %v", err)
	}
	if !sameFileContents(launcher, oldBin) {
		t.Fatal("marker-authoritative old launcher was not restored")
	}
	if current, ok := resolveCurrentVersion(layout); !ok || current != "1.0.0" {
		t.Fatalf("reconciled current = %q,%v", current, ok)
	}

	custom := []byte("custom executable that must not be overwritten")
	if err := os.WriteFile(launcher, custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reconcileActivation(layout); err == nil {
		t.Fatal("custom launcher was accepted as an interrupted managed activation")
	}
	got, err := os.ReadFile(launcher)
	if err != nil || !bytes.Equal(got, custom) {
		t.Fatalf("custom launcher was modified: got=%q err=%v", got, err)
	}
}
