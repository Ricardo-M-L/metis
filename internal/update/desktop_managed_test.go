package update

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func desktopTestCache(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("LocalAppData", filepath.Join(root, "cache"))
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "")
	t.Setenv("GOBIN", filepath.Join(root, "go-bin"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func TestDesktopManagedPathsRefuseSelfUpdateWithoutMarker(t *testing.T) {
	cache := desktopTestCache(t)
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "Métis Desktop.app", "Contents", "Resources", "bin", "metis"),
		filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("a", 64), executableName()),
	} {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			if err := CheckSelfPathSafe(path); !errors.Is(err, ErrDesktopManaged) {
				t.Fatalf("Desktop-owned path allowed self-update: path=%q err=%v", path, err)
			}
		})
	}
}

func TestDesktopManagedGuardDoesNotMatchOrdinaryPaths(t *testing.T) {
	cache := desktopTestCache(t)
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "bin", executableName()),
		filepath.Join(root, "my-app", "Contents", "Resources", "bin", "metis"),
		filepath.Join(root, "notes.app", "bin", "metis"),
		filepath.Join(root, "metis-desktop", "runtime", strings.Repeat("a", 64), executableName()),
		filepath.Join(cache, "metis-desktop", "runtime", "release", executableName()),
		filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("A", 64), executableName()),
		filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("a", 63), executableName()),
		filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("a", 64), "bin", executableName()),
	} {
		if err := CheckSelfPathSafe(path); err != nil {
			t.Errorf("ordinary path rejected: %q: %v", path, err)
		}
	}
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "true")
	if err := CheckSelfPathSafe(filepath.Join(root, executableName())); err != nil {
		t.Fatalf("unrecognized marker was treated as Desktop ownership: %v", err)
	}
}

func TestDesktopManagedMarkerRefusesSelfUpdate(t *testing.T) {
	desktopTestCache(t)
	t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", "1")
	if err := CheckSelfPathSafe(filepath.Join(t.TempDir(), executableName())); !errors.Is(err, ErrDesktopManaged) {
		t.Fatalf("Desktop child marker ignored: %v", err)
	}
}

func TestDesktopManagedSymlinkTargetRefusesSelfUpdate(t *testing.T) {
	desktopTestCache(t)
	root := t.TempDir()
	bundled := filepath.Join(root, "Metis.app", "Contents", "Resources", "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(bundled), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundled, []byte("bundled"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "metis-link")
	if err := os.Symlink(bundled, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := CheckSelfPathSafe(link); !errors.Is(err, ErrDesktopManaged) {
		t.Fatalf("bundle target hidden behind a symlink: %v", err)
	}
}

func TestDesktopManagedApplyRejectsBeforeNetworkOrFilesystemMutation(t *testing.T) {
	cache := desktopTestCache(t)
	root := t.TempDir()
	for _, tc := range []struct {
		name, path, marker string
	}{
		{"app", filepath.Join(root, "Metis.app", "Contents", "Resources", "bin", "metis"), ""},
		{"cache", filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("b", 64), executableName()), ""},
		{"marker", filepath.Join(root, "marker", executableName()), "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METIS_DESKTOP_BUNDLED_RUNTIME", tc.marker)
			for _, automatic := range []bool{false, true} {
				if err := os.MkdirAll(filepath.Dir(tc.path), 0755); err != nil {
					t.Fatal(err)
				}
				original := []byte("Desktop runtime must remain immutable")
				if err := os.WriteFile(tc.path, original, 0755); err != nil {
					t.Fatal(err)
				}
				_, layout, ok := managedLayoutForApply(tc.path)
				if !ok {
					t.Fatal("test path was not accepted by old updater layout")
				}
				requests := 0
				useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
					requests++
					http.Error(w, "forbidden update request", http.StatusInternalServerError)
				})
				var err error
				if automatic {
					_, err = ApplyIfNeeded(context.Background(), "", tc.path, nil)
				} else {
					err = Apply(context.Background(), "", tc.path, nil)
				}
				if !errors.Is(err, ErrDesktopManaged) || requests != 0 {
					t.Errorf("automatic=%t: err=%v requests=%d; want Desktop guard and zero requests", automatic, err, requests)
				}
				if _, err := os.Lstat(layout.managedRoot); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("automatic=%t created update state beside Desktop runtime: %s: %v", automatic, layout.managedRoot, err)
				}
				if current, err := os.ReadFile(tc.path); err != nil || !reflect.DeepEqual(current, original) {
					t.Fatalf("Desktop runtime changed: %q: %v", current, err)
				}
			}
		})
	}
}

func TestDesktopManagedMaintenanceNeverTouchesRuntime(t *testing.T) {
	cache := desktopTestCache(t)
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "Metis.app", "Contents", "Resources", "bin", "metis"),
		filepath.Join(cache, "metis-desktop", "runtime", strings.Repeat("c", 64), executableName()),
	} {
		if err := CleanupManaged(context.Background(), path); !errors.Is(err, ErrDesktopManaged) {
			t.Fatalf("Desktop cleanup must report the ownership guard: %v", err)
		}
		release, err := registerRunningVersion(path, "0.4.88", path)
		if err != nil || release == nil {
			t.Fatalf("Desktop startup registration should be a no-op: %v", err)
		}
		release()
		if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("maintenance created a Desktop runtime path: %v", err)
		}
	}
}

func TestDesktopManagedVersionAliasCannotBypassFinalGuard(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix app bundle launcher layout")
	}
	desktopTestCache(t)
	for _, operation := range []string{"apply", "auto-apply", "cleanup", "register"} {
		t.Run(operation, func(t *testing.T) {
			launcher := filepath.Join(t.TempDir(), "Metis.app", "Contents", "Resources", "bin", "metis")
			if err := os.MkdirAll(filepath.Dir(launcher), 0755); err != nil {
				t.Fatal(err)
			}
			layout := layoutForLauncher(launcher)
			versioned := seedManagedVersion(t, layout.versionsRoot, "0.4.88", time.Now())
			replaceLauncherSymlink(t, launcher, versioned)
			requests := 0
			useReleaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				http.Error(w, "forbidden update request", http.StatusInternalServerError)
			})
			var err error
			switch operation {
			case "apply":
				err = Apply(context.Background(), "", versioned, nil)
			case "auto-apply":
				_, err = ApplyIfNeeded(context.Background(), "", versioned, nil)
			case "cleanup":
				err = CleanupManaged(context.Background(), versioned)
			case "register":
				var release func()
				release, err = registerRunningVersion(versioned, "0.4.88", versioned)
				if err != nil || release == nil {
					t.Fatalf("Desktop registration should be a no-op: %v", err)
				}
				release()
			}
			if operation != "register" && !errors.Is(err, ErrDesktopManaged) {
				t.Errorf("normalized Desktop launcher bypassed final guard: %v", err)
			}
			if requests != 0 {
				t.Errorf("normalized Desktop launcher made %d remote calls", requests)
			}
			if _, err := os.Lstat(layout.locksRoot); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("normalized Desktop launcher created updater state: %v", err)
			}
		})
	}
}
