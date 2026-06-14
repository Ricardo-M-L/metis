package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store persists named workflows as JSON files under a directory
// (typically ~/.metis/workflows). Named workflows let common multi-step
// sequences (build→test→lint) be saved once and re-run by name.
type Store struct {
	dir string
}

// NewStore returns a Store rooted at dir. The directory is created
// lazily on first Save.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// validName guards against path traversal and keeps filenames sane:
// a workflow name is a single path segment of [A-Za-z0-9_-].
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}

// Save writes wf under its Name. Returns an error for an invalid name.
func (s *Store) Save(wf Workflow) error {
	if !validName(wf.Name) {
		return fmt.Errorf("invalid workflow name %q (use letters, digits, _ or -)", wf.Name)
	}
	if len(wf.Steps) == 0 {
		return fmt.Errorf("workflow %q has no steps", wf.Name)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(wf.Name), b, 0o644)
}

// Load reads a named workflow. Returns a clear error when absent.
func (s *Store) Load(name string) (Workflow, error) {
	if !validName(name) {
		return Workflow{}, fmt.Errorf("invalid workflow name %q", name)
	}
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return Workflow{}, fmt.Errorf("no saved workflow named %q", name)
		}
		return Workflow{}, err
	}
	var wf Workflow
	if err := json.Unmarshal(b, &wf); err != nil {
		return Workflow{}, fmt.Errorf("workflow %q is corrupt: %w", name, err)
	}
	return wf, nil
}

// List returns the names of saved workflows, sorted.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}
