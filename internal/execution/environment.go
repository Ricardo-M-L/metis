// Package execution keeps a small, durable record of workspace capabilities
// that affect how METIS can launch sub-agents.
//
// It deliberately records deterministic runtime facts rather than model-made
// summaries. A workspace that is not a Git repository cannot provide a Git
// worktree, regardless of how many times a model asks for one. Retaining that
// fact lets the Agent tool normalize the next equivalent request before it
// becomes another failed dispatch.
package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FailureCode identifies an environment constraint. Values are stable because
// they are persisted and may later be rendered in the Desktop trace.
type FailureCode string

const (
	// FailureWorktreeRequiresGit means a caller requested Git worktree
	// isolation for a directory that Git cannot resolve to a repository.
	// Agent can safely recover this one case by preserving cwd and using direct
	// execution, subject to the parent permission gate.
	FailureWorktreeRequiresGit FailureCode = "worktree_requires_git"
	// FailureNestedWorktree means the source itself is a linked worktree. METIS
	// does not create worktree-of-worktree descendants because ownership and
	// cleanup would become ambiguous. This fact is remembered but never
	// auto-recovered into a write-capable direct execution.
	FailureNestedWorktree FailureCode = "nested_worktree"
)

// Failure is the durable explanation for a capability constraint. Summary and
// SuggestedAction are controlled strings, never raw model input or shell
// output, so the store does not become a second transcript.
type Failure struct {
	Code            FailureCode `json:"code"`
	Summary         string      `json:"summary"`
	SuggestedAction string      `json:"suggested_action,omitempty"`
	AutoRecovered   bool        `json:"auto_recovered"`
	Occurrences     int         `json:"occurrences"`
	FirstObservedAt time.Time   `json:"first_observed_at"`
	LastObservedAt  time.Time   `json:"last_observed_at"`
}

// Profile is a point-in-time view of the execution environment for one
// workspace. It is intentionally small: enough to choose isolation safely,
// without duplicating Git state or user project contents.
type Profile struct {
	Version          int       `json:"version"`
	Workspace        string    `json:"workspace"`
	GitRoot          string    `json:"git_root,omitempty"`
	IsGitRepository  bool      `json:"is_git_repository"`
	IsLinkedWorktree bool      `json:"is_linked_worktree"`
	ObservedAt       time.Time `json:"observed_at"`
	LastFailure      *Failure  `json:"last_failure,omitempty"`
}

// Memory is the narrow persistence contract used by the Agent tool. It makes
// production storage injectable and leaves embedded callers free to omit
// durable memory while retaining the same immediate preflight behavior.
type Memory interface {
	Load(workspace string) (Profile, bool, error)
	Record(profile Profile, failure Failure) error
}

// Store persists one capability profile per canonical workspace. Files are
// named from a hash rather than a path so the directory remains navigable
// without exposing project paths in filenames.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore returns a lazy store rooted at dir. It performs no I/O until a
// profile is recorded, so informational paths such as `metis tools` do not
// create state merely by advertising Agent.
func NewStore(dir string) *Store { return &Store{dir: strings.TrimSpace(dir)} }

// Probe observes a workspace at call time. Git is deliberately probed on each
// Agent invocation rather than trusted from an old record: a user may run
// `git init` between two turns, and an old non-Git observation must never
// permanently suppress valid worktree isolation.
func Probe(workspace string) (Profile, error) {
	canonical, err := CanonicalWorkspace(workspace)
	if err != nil {
		return Profile{}, err
	}
	profile := Profile{
		Version:    1,
		Workspace:  canonical,
		ObservedAt: time.Now().UTC(),
	}
	root, err := gitOutput(canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return profile, nil
	}
	profile.IsGitRepository = true
	profile.GitRoot = canonicalPath(root)
	gitDir, gitDirErr := gitOutput(canonical, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	commonDir, commonErr := gitOutput(canonical, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if gitDirErr == nil && commonErr == nil {
		profile.IsLinkedWorktree = canonicalPath(gitDir) != canonicalPath(commonDir)
	}
	return profile, nil
}

// CanonicalWorkspace validates an existing directory and produces the stable
// identity used by both the on-disk store and Agent recovery records.
func CanonicalWorkspace(workspace string) (string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return "", fmt.Errorf("execution workspace is empty")
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("execution workspace %q must be absolute", workspace)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return "", fmt.Errorf("execution workspace %q: %w", workspace, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("execution workspace %q is not a directory", workspace)
	}
	return canonicalPath(workspace), nil
}

// Load returns the latest record for workspace. Missing/corrupt records are
// intentionally distinguishable: a corrupt file is surfaced to the caller,
// which can keep running but should not silently claim a remembered rule.
func (s *Store) Load(workspace string) (Profile, bool, error) {
	if s == nil || s.dir == "" {
		return Profile{}, false, nil
	}
	canonical, err := CanonicalWorkspace(workspace)
	if err != nil {
		return Profile{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(canonical)
}

// Record merges a deterministic failure into the workspace record. Existing
// records for a different failure code are replaced because LastFailure means
// "the environment rule that most recently governed dispatch", while repeat
// observations of the same rule increase Occurrences.
func (s *Store) Record(profile Profile, failure Failure) error {
	if s == nil || s.dir == "" {
		return nil
	}
	canonical, err := CanonicalWorkspace(profile.Workspace)
	if err != nil {
		return err
	}
	if failure.Code == "" {
		return fmt.Errorf("execution failure code is empty")
	}
	now := time.Now().UTC()
	profile.Version = 1
	profile.Workspace = canonical
	profile.ObservedAt = now
	failure.FirstObservedAt = now
	failure.LastObservedAt = now
	failure.Occurrences = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, found, err := s.loadLocked(canonical)
	if err != nil {
		return err
	}
	if found && previous.LastFailure != nil && previous.LastFailure.Code == failure.Code {
		failure.FirstObservedAt = previous.LastFailure.FirstObservedAt
		failure.Occurrences = previous.LastFailure.Occurrences + 1
	}
	profile.LastFailure = &failure
	return s.writeLocked(profile)
}

func (s *Store) loadLocked(canonical string) (Profile, bool, error) {
	path := s.pathFor(canonical)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Profile{}, false, nil
		}
		return Profile{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Profile{}, false, fmt.Errorf("execution profile %s is not a regular file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, false, err
	}
	var profile Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		return Profile{}, false, fmt.Errorf("decode execution profile: %w", err)
	}
	if profile.Version != 1 || profile.Workspace != canonical {
		return Profile{}, false, fmt.Errorf("execution profile identity mismatch")
	}
	return profile, true, nil
}

func (s *Store) writeLocked(profile Profile) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".profile-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.pathFor(profile.Workspace))
}

func (s *Store) pathFor(canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	return filepath.Join(s.dir, hex.EncodeToString(digest[:])+".json")
}

func gitOutput(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Stderr = nil
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return filepath.Clean(strings.TrimSpace(path))
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}
