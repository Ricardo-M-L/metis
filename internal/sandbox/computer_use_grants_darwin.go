//go:build darwin

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The native helper uses exactly this directory and temporary-file rename.
// Keep the capability path-free, and never resolve state symlinks into new
// grants. The home passed here was already canonicalized by Manager.Wrap.
func computerUseDarwinAppGrantsProfile(home string, enabled bool) (string, error) {
	if home == "" && !enabled {
		return "", nil
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) == "/" {
		return "", errors.New("sandbox: Computer Use app grants require a resolved user home")
	}
	dir := filepath.Join(home, ".metis-cu")
	path := filepath.Join(dir, "granted.json")
	quote := func(path string) string {
		path = strings.ReplaceAll(path, `\`, `\\`)
		path = strings.ReplaceAll(path, `"`, `\"`)
		return `"` + path + `"`
	}
	if !enabled {
		// A general MCP process must not forge future app approvals, including
		// when its otherwise-writable working directory is the user's home.
		return fmt.Sprintf("(deny file-write* (subpath %s))\n", quote(dir)), nil
	}
	for _, candidate := range []string{dir, path, path + ".tmp"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("sandbox: inspect Computer Use app grants: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (candidate == dir && !info.IsDir()) || (candidate != dir && !info.Mode().IsRegular()) {
			return "", fmt.Errorf("sandbox: unsafe Computer Use app grants path %q: symlink or unexpected file type", candidate)
		}
		if candidate != dir {
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
				return "", fmt.Errorf("sandbox: unsafe Computer Use app grants path %q: hard-linked file", candidate)
			}
		}
	}
	// The base profile excludes this subtree from cwd/temp write grants, so
	// the directory receives creation only, never rename, unlink or metadata
	// writes. Deny adjacent state as a further boundary. Type
	// filters also reject creating symlinks after this preflight check. Exact
	// literal targets retain the normal credential denies; no subpath allow or
	// symlink-expanded target is introduced.
	return fmt.Sprintf(`(deny file-write*
  (require-all (subpath %[1]s)
    (require-not (literal %[1]s))
    (require-not (literal %[2]s))
    (require-not (literal %[3]s))))
(deny file-write*
  (require-all (literal %[1]s) (require-not (vnode-type DIRECTORY))))
(deny file-write*
  (require-all
    (require-any (literal %[2]s) (literal %[3]s))
    (require-not (vnode-type REGULAR-FILE))))
(allow file-write-create
  (require-all (literal %[1]s) (vnode-type DIRECTORY)))
(allow file-write-create file-write-data file-write-unlink
  (require-all
    (require-any (literal %[2]s) (literal %[3]s))
    (vnode-type REGULAR-FILE)))
`, quote(dir), quote(path), quote(path+".tmp")), nil
}
