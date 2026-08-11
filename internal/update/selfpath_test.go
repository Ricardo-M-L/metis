package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveSelfPathPrefersVerifiedLauncherWhenExecutableIsResolvedTarget(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix symlink path regression")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	target := seedManagedVersion(t, layout.versionsRoot, "1.2.3", time.Unix(1, 0))
	replaceLauncherSymlink(t, launcher, target)

	got, err := resolveSelfPath(launcher, target, func(string) (string, error) {
		t.Fatal("lookPath should not be needed for an absolute argv0")
		return "", nil
	})
	if err != nil {
		t.Fatalf("resolveSelfPath: %v", err)
	}
	if got != launcher {
		t.Fatalf("resolveSelfPath = %q, want stable launcher %q", got, launcher)
	}
}

func TestResolveSelfPathDerivesLauncherFromManagedExecutable(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix managed layout regression")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	target := seedManagedVersion(t, layout.versionsRoot, "1.2.3", time.Unix(1, 0))
	replaceLauncherSymlink(t, launcher, target)

	got, err := resolveSelfPath("metis-not-on-path", target, func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatalf("resolveSelfPath: %v", err)
	}
	if got != launcher {
		t.Fatalf("resolveSelfPath = %q, want derived launcher %q", got, launcher)
	}
}

func TestResolveSelfPathNeverReturnsUnverifiedManagedVersionTarget(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix managed layout regression")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	layout := layoutForLauncher(launcher)
	target := seedManagedVersion(t, layout.versionsRoot, "1.2.3", time.Unix(1, 0))

	got, err := resolveSelfPath("metis-not-on-path", target, func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err == nil {
		t.Fatalf("resolveSelfPath unexpectedly returned %q for an unverified managed target", got)
	}
}

func TestResolveSelfPathReturnsLauncherWhenItAlreadyPointsToNewerVersion(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix managed layout regression")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	oldRunning := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Unix(1, 0))
	newCurrent := seedManagedVersion(t, layout.versionsRoot, "2.0.0", time.Unix(2, 0))
	replaceLauncherSymlink(t, launcher, newCurrent)

	got, err := resolveSelfPath("not-on-path", oldRunning, func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatalf("resolveSelfPath old running/new current: %v", err)
	}
	if got != launcher {
		t.Fatalf("resolveSelfPath = %q, want stable launcher %q", got, launcher)
	}
}
