package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

// Store reads the learning activation authority before legacy JSON baselines
// (typically ~/.metis/workflows). Model proposals remain separate until a host
// evaluation promotes them; legacy host-authored recipes remain supported.
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
	if learning.ValidateName(name) == nil {
		state, err := s.LearningStore().Read(name)
		if err == nil {
			if state.Suspended {
				return Workflow{}, fmt.Errorf("workflow %q suspended by host quality guard; host review and resume required", name)
			}
			if state.ActiveVersion == "" {
				return Workflow{}, fmt.Errorf("workflow %q has only candidates; evaluate and promote before running", name)
			}
			v := state.Versions[state.ActiveVersion]
			wf, err := ParseManifest(name, []byte(v.Content))
			wf.VersionSHA256 = v.ID
			return wf, err
		}
		if !os.IsNotExist(err) {
			return Workflow{}, err
		}
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
	wf.VersionSHA256 = learning.Digest(b)
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
	states, err := s.LearningStore().List()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	for _, state := range states {
		if state.ActiveVersion != "" && !seen[state.Name] {
			names = append(names, state.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}
