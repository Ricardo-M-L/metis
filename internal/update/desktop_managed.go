package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrDesktopManaged identifies a runtime owned by a Desktop installation.
// Replacing it independently would invalidate the app signature or corrupt its
// content-addressed runtime cache. Desktop updates replace the whole runtime.
var ErrDesktopManaged = errors.New("Metis Desktop manages this runtime; update Metis Desktop instead of running `metis update`")

func checkDesktopManagedPath(path string) error {
	if os.Getenv("METIS_DESKTOP_BUNDLED_RUNTIME") == "1" {
		return ErrDesktopManaged
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	paths := []string{filepath.Clean(abs)}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		paths = append(paths, resolved)
	}
	// SelfPath can recover a launcher alias (including METIS_INSTALL_DIR).
	// The executing image remains Desktop-owned even if that alias is outside
	// the app bundle, so it cannot authorize installing a second runtime.
	if executable, err := os.Executable(); err == nil && executable != abs {
		paths = append(paths, filepath.Clean(executable))
		if resolved, err := filepath.EvalSymlinks(executable); err == nil && resolved != executable {
			paths = append(paths, resolved)
		}
	}
	var caches []string
	if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		caches = append(caches, filepath.Clean(cache))
		if resolved, err := filepath.EvalSymlinks(cache); err == nil && resolved != cache {
			caches = append(caches, resolved)
		}
	}
	for _, candidate := range paths {
		if desktopBundlePath(candidate) {
			return ErrDesktopManaged
		}
		for _, cache := range caches {
			if desktopCachePath(cache, candidate) {
				return ErrDesktopManaged
			}
		}
	}
	return nil
}

func desktopBundlePath(path string) bool {
	// Match the actual packaged sidecar layout rather than any directory
	// containing "Desktop" or ".app" in its name.
	for _, name := range []string{"metis", "bin", "Resources", "Contents"} {
		if filepath.Base(path) != name {
			return false
		}
		path = filepath.Dir(path)
	}
	app := filepath.Base(path)
	return len(app) > len(".app") && strings.HasSuffix(app, ".app")
}

func desktopCachePath(cache, path string) bool {
	rel, err := filepath.Rel(cache, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 4 || parts[0] != "metis-desktop" || parts[1] != "runtime" || parts[3] != executableName() || len(parts[2]) != 64 {
		return false
	}
	for _, c := range parts[2] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
