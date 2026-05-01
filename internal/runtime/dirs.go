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

// AllowedDirs is the set of directories the user has explicitly opted into
// beyond the launch cwd. Tools like Read/Write/Edit/Bash do not currently
// enforce a cwd boundary in metis (absolute paths work everywhere), so
// AllowedDirs serves two purposes today:
//
//   - Surfaces the list to the LLM via system prompt — the agent learns
//     it has authority to operate in those locations and won't act
//     overly cautious about them.
//   - Provides the persistence + plumbing that a future permission-gate
//     enforcement pass can hook into without needing to refactor the
//     /add-dir UI.
//
// Concurrency: list is rebuilt as a fresh slice on each Add/Remove, so
// readers can hold the slice without further locking.
type AllowedDirs struct {
	mu        sync.RWMutex
	dirs      []string // absolute, deduped, sorted
	persistTo string   // ~/.metis/additional-dirs.json
}

// NewAllowedDirs constructs an instance backed by ~/.metis/additional-dirs.json.
// Pre-populates from CLI-passed `extras` (typically `--add-dir` values) +
// any persisted list. Errors loading the persisted file are non-fatal —
// a corrupt file shouldn't kill chat startup.
func NewAllowedDirs(extras []string) *AllowedDirs {
	d := &AllowedDirs{
		persistTo: filepath.Join(config.Home(), dirsFileName),
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
	return abs, nil
}
