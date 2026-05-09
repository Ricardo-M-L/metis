// Package projects maintains a global registry of directories where
// metis has been used. Stored at ~/.metis/projects.json, the registry
// is the data backing for `metis projects` (list + sort by last
// access time).
//
// Mirrors crush's internal/projects/projects.go layout:
//
//	{
//	  "projects": [
//	    {"path": "/Users/x/code/api", "data_dir": "/Users/x/.metis", "last_accessed": "..."},
//	    ...
//	  ]
//	}
//
// Register() is called from the metis startup path (cmd/metis/main.go)
// once a session decides to use the cwd as its working dir. Idempotent:
// the same path bumps `last_accessed` instead of duplicating.
//
// Concurrency: a package-level mutex serialises Load/Save. Cross-
// process races are not addressed (two metis instances starting at the
// same instant could clobber each other's update). Acceptable: the
// registry is non-critical metadata; loss of a single bump just means
// one project doesn't sort to top until the next access.
package projects

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

const projectsFile = "projects.json"

// Project is one tracked directory. Path is the absolute working
// directory; DataDir is the metis home that served the session
// (currently always config.Home(), kept here so per-project home
// overrides via $METIS_HOME show up clearly in `metis projects`).
type Project struct {
	Path         string    `json:"path"`
	DataDir      string    `json:"data_dir"`
	LastAccessed time.Time `json:"last_accessed"`
}

// Registry is the on-disk file's shape. Wrapped in a struct rather
// than the bare slice so future additions (schema_version, last
// session id, etc.) don't need a migration.
type Registry struct {
	Projects []Project `json:"projects"`
}

var mu sync.Mutex

// Path returns the canonical projects.json path for the current
// metis home. Resolved each call so a $METIS_HOME override in a
// shell-scoped env var wins.
func Path() string {
	return filepath.Join(config.Home(), projectsFile)
}

// Load reads the registry from disk. A missing file returns an empty
// registry, not an error — registries are user-state, not config.
func Load() (*Registry, error) {
	mu.Lock()
	defer mu.Unlock()
	return loadLocked()
}

func loadLocked() (*Registry, error) {
	path := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Registry{}, nil
		}
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Save writes the registry atomically (temp file + rename) so a
// crash mid-write doesn't leave a half-written JSON. 0o600 perms
// because the path list could leak "what projects this user is
// working on" — useful private info.
func Save(r *Registry) error {
	mu.Lock()
	defer mu.Unlock()
	return saveLocked(r)
}

func saveLocked(r *Registry) error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Register adds or refreshes the entry for workingDir. Updates
// LastAccessed to time.Now() and resorts the list. dataDir is the
// metis home in effect — stored so a user with multiple METIS_HOMEs
// can see which one was used per project.
//
// No-op when workingDir is empty (defensive — a misconfigured caller
// shouldn't poison the registry with anonymous entries).
func Register(workingDir, dataDir string) error {
	if workingDir == "" {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	r, err := loadLocked()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	updated := false
	for i := range r.Projects {
		if r.Projects[i].Path == workingDir {
			r.Projects[i].DataDir = dataDir
			r.Projects[i].LastAccessed = now
			updated = true
			break
		}
	}
	if !updated {
		r.Projects = append(r.Projects, Project{
			Path: workingDir, DataDir: dataDir, LastAccessed: now,
		})
	}
	sortByRecency(r.Projects)
	return saveLocked(r)
}

// List returns all tracked projects, most-recently-accessed first.
func List() ([]Project, error) {
	r, err := Load()
	if err != nil {
		return nil, err
	}
	sortByRecency(r.Projects)
	return r.Projects, nil
}

// sortByRecency stable-sorts projects newest-first.
func sortByRecency(ps []Project) {
	sort.SliceStable(ps, func(i, j int) bool {
		return ps[i].LastAccessed.After(ps[j].LastAccessed)
	})
}

// Remove deletes the entry for workingDir. No-op when not present.
// Returns true when an entry was actually removed.
func Remove(workingDir string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()
	r, err := loadLocked()
	if err != nil {
		return false, err
	}
	out := r.Projects[:0]
	removed := false
	for _, p := range r.Projects {
		if p.Path == workingDir {
			removed = true
			continue
		}
		out = append(out, p)
	}
	if !removed {
		return false, nil
	}
	r.Projects = out
	return true, saveLocked(r)
}
