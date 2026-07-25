package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// dirsFileName lives under ~/.metis/ and stores the persisted set of
// additional accessible directories (claude-code's settings.local.json
// equivalent for the metis "--add-dir" feature).
const dirsFileName = "additional-dirs.json"

// AllowedDirs is the filesystem scope granted to one Metis process. The launch
// cwd is always in scope; dirs contains the additional roots explicitly opted
// into through --add-dir, /add-dir, or the persisted local list.
//
// Concurrency: list is rebuilt as a fresh slice on each Add/Remove, so
// readers can hold the slice without further locking.
type AllowedDirs struct {
	mu        sync.RWMutex
	cwd       string   // canonical launch cwd; implicit and never persisted
	dirs      []string // absolute, deduped, sorted
	persistTo string   // ~/.metis/additional-dirs.json
}

// NewAllowedDirs constructs an instance backed by ~/.metis/additional-dirs.json.
// Pre-populates from CLI-passed `extras` (typically `--add-dir` values) +
// any persisted list. Errors loading the persisted file are non-fatal —
// a corrupt file shouldn't kill chat startup.
func NewAllowedDirs(extras []string) *AllowedDirs {
	cwd, _ := os.Getwd()
	return newAllowedDirs(cwd, extras)
}

// newAllowedDirs is the deterministic constructor used by tests. Keeping the
// launch cwd on the object (instead of consulting os.Getwd in Contains) also
// prevents a later chdir from silently changing the permission boundary.
func newAllowedDirs(cwd string, extras []string) *AllowedDirs {
	d := &AllowedDirs{
		persistTo: filepath.Join(config.Home(), dirsFileName),
	}
	if root, err := normalizeDir(cwd); err == nil {
		d.cwd = root
	}
	d.loadFromDisk()
	for _, p := range extras {
		_ = d.add(p, false) // session-only by default for CLI flag
	}
	return d
}

// All returns a snapshot of the allowed directories. Safe to retain.
func (d *AllowedDirs) All() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.dirs))
	copy(out, d.dirs)
	return out
}

// Scope returns every effective root, including the implicit launch cwd.
// All intentionally remains the additional-only view used by /list-dirs and
// persistence, while Scope is the actual permission boundary.
func (d *AllowedDirs) Scope() []string {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.dirs)+1)
	if d.cwd != "" {
		out = append(out, d.cwd)
	}
	for _, dir := range d.dirs {
		if dir != d.cwd {
			out = append(out, dir)
		}
	}
	return out
}

// Contains reports whether path resolves under the launch cwd or an explicit
// additional root. Relative paths are rooted at the launch cwd. Resolution is
// symlink-aware even for not-yet-created write targets: each existing path
// component is lstat'd and a dangling symlink's lexical target is followed, so
// `cwd/link -> /outside/new-file` cannot masquerade as an in-scope path.
//
// Empty paths are left to the tool's schema/validation layer. Treating them as
// in-scope preserves its useful "path is required" error instead of opening a
// misleading permission prompt.
func (d *AllowedDirs) Contains(path string) bool {
	if d == nil {
		return false
	}
	if strings.TrimSpace(path) == "" {
		return true
	}

	d.mu.RLock()
	cwd := d.cwd
	roots := make([]string, 0, len(d.dirs)+1)
	if cwd != "" {
		roots = append(roots, cwd)
	}
	roots = append(roots, d.dirs...)
	d.mu.RUnlock()

	if !filepath.IsAbs(path) {
		if cwd == "" {
			return false
		}
		path = filepath.Join(cwd, path)
	}
	resolved, err := resolvePath(path, 0)
	if err != nil {
		return false
	}
	for _, root := range roots {
		if pathWithin(root, resolved) {
			return true
		}
	}
	return false
}

// Add appends `path` to the allowed set, normalizing to an absolute path
// and verifying the directory exists. When `persist=true` the new set is
// written to disk so it survives restarts.
func (d *AllowedDirs) Add(path string, persist bool) error {
	return d.add(path, persist)
}

func (d *AllowedDirs) add(path string, persist bool) error {
	abs, err := normalizeDir(path)
	if err != nil {
		return err
	}
	d.mu.Lock()
	for _, existing := range d.dirs {
		if existing == abs {
			d.mu.Unlock()
			return nil // already there, no error
		}
	}
	d.dirs = append(d.dirs, abs)
	sort.Strings(d.dirs)
	d.mu.Unlock()
	if persist {
		return d.saveToDisk()
	}
	return nil
}

// Remove drops `path` from the allowed set. Persists if currently persisted.
func (d *AllowedDirs) Remove(path string) error {
	abs, err := normalizeDir(path)
	if err != nil {
		// Allow removal even if dir no longer exists on disk; just normalize the spelling.
		abs = filepath.Clean(path)
	}
	d.mu.Lock()
	out := d.dirs[:0]
	found := false
	for _, p := range d.dirs {
		if p == abs {
			found = true
			continue
		}
		out = append(out, p)
	}
	d.dirs = out
	d.mu.Unlock()
	if !found {
		return fmt.Errorf("not in allowed dirs: %s", abs)
	}
	return d.saveToDisk()
}

func (d *AllowedDirs) loadFromDisk() {
	b, err := os.ReadFile(d.persistTo)
	if err != nil {
		return
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}
	for _, p := range list {
		abs, err := normalizeDir(p)
		if err != nil {
			continue
		}
		d.dirs = append(d.dirs, abs)
	}
	sort.Strings(d.dirs)
}

func (d *AllowedDirs) saveToDisk() error {
	if err := os.MkdirAll(filepath.Dir(d.persistTo), 0o755); err != nil {
		return err
	}
	d.mu.RLock()
	out := make([]string, len(d.dirs))
	copy(out, d.dirs)
	d.mu.RUnlock()
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.persistTo, b, 0o644)
}

// SystemPromptAddendum renders the directory list as a one-paragraph
// addendum suitable to append to the agent's system prompt. Empty when
// no extra dirs are configured.
func (d *AllowedDirs) SystemPromptAddendum() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.dirs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAdditional accessible directories (treat these as in-scope alongside cwd):\n")
	for _, p := range d.dirs {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func normalizeDir(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	resolved, err := resolvePath(abs, 0)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", abs, err)
	}
	return resolved, nil
}

// pathWithin is component-aware: /repo2 is not inside /repo.
func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// resolvePath canonicalizes symlinks without requiring the final target to
// exist. filepath.EvalSymlinks cannot resolve a dangling link, yet dangling
// links matter for Write: os.WriteFile follows them once their external parent
// exists. Walking one component at a time closes that escape while still
// allowing permission checks for new files.
func resolvePath(path string, hops int) (string, error) {
	if hops > 255 {
		return "", fmt.Errorf("too many symlinks resolving %s", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)

	volume := filepath.VolumeName(abs)
	root := volume + string(os.PathSeparator)
	rest := strings.TrimPrefix(abs, root)
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == os.PathSeparator })
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, lerr := os.Lstat(current)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				remaining := append([]string{current}, parts[i+1:]...)
				return filepath.Clean(filepath.Join(remaining...)), nil
			}
			return "", lerr
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, rerr := os.Readlink(current)
		if rerr != nil {
			return "", rerr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		next := append([]string{target}, parts[i+1:]...)
		return resolvePath(filepath.Join(next...), hops+1)
	}
	return filepath.Clean(current), nil
}
