// Package checkpoint manages a shadow-git snapshot of the working
// tree so users can /rollback to any previous tool-mutation point.
//
// Design (mirrors hermes-agent's checkpoint_manager.py + minimal):
//
//   - Per-session shadow repo at ~/.metis/checkpoints/<session-id>/
//   - Each Edit/Write/Bash that mutates files triggers Snap()
//   - Snap copies tracked files into the shadow tree, commits with
//     metadata: {tool_name, args_hash, time, message}
//   - Restore(N) checks out the Nth-most-recent commit's tree back
//     into the working dir
//   - List() returns the recent commit log for the /rollback picker
//
// Why a shadow repo instead of mutating the user's real .git: many
// metis sessions run inside a project that's already a git repo,
// and we don't want our auto-commit-every-tool-call to pollute the
// user's commit history (or worse, conflict with branches they
// haven't pushed). Shadow repo is a sibling, isolated.
//
// Limitations:
//   - only tracks the current cwd subtree (not the whole repo)
//   - binary blobs > 1 MiB are skipped (don't bloat the shadow)
//   - doesn't track deletions of files NOT yet snapped
package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Manager owns one shadow repo for one session.
type Manager struct {
	mu        sync.Mutex
	sessionID string
	shadowDir string // ~/.metis/checkpoints/<session-id>
	cwd       string // the working dir whose state we mirror
	disabled  bool   // turns off all operations after a fatal init error
	initOnce  sync.Once
	initErr   error
}

// NewManager constructs a Manager for the given session id.
// The shadow repo is initialized lazily (first Snap call) so we
// don't pay the cost on sessions that never mutate anything.
//
// shadowRoot defaults to ~/.metis/checkpoints when empty.
func NewManager(sessionID, cwd, shadowRoot string) *Manager {
	if shadowRoot == "" {
		if home, err := os.UserHomeDir(); err == nil {
			shadowRoot = filepath.Join(home, ".metis", "checkpoints")
		} else {
			shadowRoot = "/tmp/metis-checkpoints"
		}
	}
	m := &Manager{
		sessionID: sessionID,
		shadowDir: filepath.Join(shadowRoot, sessionID),
		cwd:       cwd,
	}
	// Disable checkpointing when cwd is too broad to snapshot — the home
	// directory or the filesystem root. Checkpoint exists to undo edits
	// inside a PROJECT working tree; snapshotting the entire home tree
	// is never the intent, produces a huge shadow copy, and that copy
	// then slows down later `find ~`-style commands (and, pre-v0.2.1,
	// recursed into itself). Running metis from ~ is a misuse; fail the
	// checkpoint quietly rather than mirroring the whole home dir.
	// 2026-06-14.
	if isUnsafeCheckpointRoot(cwd) {
		m.disabled = true
	}
	return m
}

// isUnsafeCheckpointRoot reports whether cwd is too broad to checkpoint:
// the filesystem root or the user's home directory itself. Sub-dirs of
// home (e.g. ~/Documents/project) are fine — only the bare roots are
// rejected.
func isUnsafeCheckpointRoot(cwd string) bool {
	if cwd == "" {
		return true
	}
	clean := filepath.Clean(cwd)
	if clean == "/" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	cleanHome := filepath.Clean(home)
	if clean == cleanHome {
		return true
	}
	// Symlink/firmlink resolution: on macOS the home dir is reachable
	// as both /Users/x and /System/Volumes/Data/Users/x, and a user may
	// have a symlinked home. A bare string compare misses those, letting
	// the whole-home snapshot slip through. Resolve both and compare —
	// best-effort, falls back to the string compare above on error.
	if rc, e1 := filepath.EvalSymlinks(clean); e1 == nil {
		if rh, e2 := filepath.EvalSymlinks(cleanHome); e2 == nil && rc == rh {
			return true
		}
	}
	return false
}

// Disabled reports whether checkpointing is off for this manager (init
// error, or an unsafe cwd root). Lets runtime warn the user that
// /rewind won't be available this session.
func (m *Manager) Disabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disabled
}

// initShadowRepo lazily creates and `git init`s the shadow dir.
// Called on first Snap. Errors mark the manager as disabled so
// subsequent Snap calls fail-fast without re-attempting.
func (m *Manager) initShadowRepo() error {
	m.initOnce.Do(func() {
		if err := os.MkdirAll(m.shadowDir, 0o700); err != nil {
			m.initErr = fmt.Errorf("checkpoint: mkdir shadow: %w", err)
			return
		}
		// Idempotent: if .git already exists we trust it.
		gitDir := filepath.Join(m.shadowDir, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			return
		}
		cmd := exec.Command("git", "init", "--quiet")
		cmd.Dir = m.shadowDir
		if out, err := cmd.CombinedOutput(); err != nil {
			m.initErr = fmt.Errorf("checkpoint: git init: %v: %s", err, string(out))
			return
		}
		// Stamp identity so commits don't fail on a fresh box where
		// `user.email` isn't set globally. Metis-as-author makes the
		// log easy to recognise.
		_ = m.git("config", "user.email", "metis@local")
		_ = m.git("config", "user.name", "metis")
	})
	if m.initErr != nil {
		m.disabled = true
		return m.initErr
	}
	return nil
}

// git runs a git command in the shadow dir. Returns combined output.
func (m *Manager) git(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.shadowDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// gitOutput runs git and returns stdout (trimmed). Used by List.
func (m *Manager) gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.shadowDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOutputBytes is the binary-safe variant used by NUL-delimited path
// commands. File names are not generally safe to split on newlines.
func (m *Manager) gitOutputBytes(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.shadowDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Snap takes a checkpoint with the given tool name + a short args
// hash + an optional human message. Returns the resulting commit
// hash (or empty when nothing changed since the last snap).
//
// Body of the snap: copy every file under m.cwd into the shadow
// dir, mirroring the relative path layout. Symlinks, files >1MiB,
// and files in skipDirs (.git, node_modules, etc.) are skipped.
//
// Errors are non-fatal — caller should log and continue. The
// agent loop must not crash because checkpointing failed.
func (m *Manager) Snap(toolName, argsHash, message string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return "", errors.New("checkpoint: disabled (prior init error)")
	}
	if err := m.initShadowRepo(); err != nil {
		return "", err
	}
	if err := m.copyTree(); err != nil {
		return "", err
	}
	if err := m.git("add", "-A"); err != nil {
		return "", err
	}
	// An empty project's first pre-edit snapshot still needs a materialized
	// commit: it is the only state capable of expressing "this file did not
	// exist" when the first tool creates it. Later unchanged snapshots remain
	// no-ops so the history does not fill with empty commits.
	_, headErr := m.gitOutput("rev-parse", "--verify", "HEAD")
	firstCommit := headErr != nil
	if hasChanges, _ := m.gitOutput("diff", "--cached", "--name-only"); hasChanges == "" && !firstCommit {
		return "", nil
	}
	commitMsg := fmt.Sprintf("%s|%s|%s|%s",
		time.Now().UTC().Format(time.RFC3339),
		toolName,
		argsHash,
		message,
	)
	commitArgs := []string{"commit", "-q", "-m", commitMsg}
	if firstCommit {
		commitArgs = append(commitArgs, "--allow-empty")
	}
	if err := m.git(commitArgs...); err != nil {
		return "", err
	}
	out, err := m.gitOutput("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return out, nil
}

// skipDirs are the cwd subtrees we never copy into the shadow.
// Tuned for typical projects.
//
// `.metis` is CRITICAL here, not just an optimization: the shadow repo
// lives at ~/.metis/checkpoints/<id>. When metis runs with cwd at (or
// above) ~/.metis — e.g. launched from the home directory — copyTree
// would otherwise Walk straight into the shadow dir it is writing to,
// copying the checkpoint tree into itself. Each snapshot then nests
// another `.metis/checkpoints/<id>/` layer until paths blow past the
// 255-byte filename limit ("file name too long"), spamming errors and
// hanging the turn (2026-06-14 user report). `.augmentcode` is excluded
// for the same reason — its checkpoint-documents store holds very long
// path-encoded filenames that compound the overflow.
var skipDirs = map[string]bool{
	".git":         true,
	".metis":       true,
	".augmentcode": true,
	"node_modules": true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".cache":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".next":        true,
	".tox":         true,
}

// maxFileBytes caps individual files we'll copy. 1 MiB strikes a
// balance: most code files are well under, large binaries/blobs
// (PDFs, images, vendored binaries) are skipped.
const maxFileBytes = 1 << 20

// copyTree mirrors m.cwd into m.shadowDir. Files are copied; dirs
// are recreated. Files removed from m.cwd between snaps are removed
// from the shadow too (handled by `git add -A` on the shadow side).
func (m *Manager) copyTree() error {
	// Inventory the eligible live tree before mutating the mirror. This makes
	// path-type replacement deterministic and ensures a walk/read failure
	// aborts Snap instead of committing a mixture of old and new files.
	liveFiles := make(map[string]string)
	liveDirs := make([]string, 0)
	if err := filepath.Walk(m.cwd, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("checkpoint: walk %s: %w", path, walkErr)
		}
		if info == nil {
			return fmt.Errorf("checkpoint: walk %s: missing file info", path)
		}
		rel, err := filepath.Rel(m.cwd, path)
		if err != nil {
			return fmt.Errorf("checkpoint: relative path for %s: %w", path, err)
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			if m.shadowDir != "" && (path == m.shadowDir ||
				strings.HasPrefix(path, m.shadowDir+string(os.PathSeparator)) ||
				strings.HasPrefix(m.shadowDir, path+string(os.PathSeparator))) {
				return filepath.SkipDir
			}
			normalized, ok := m.managedRelativePath(path)
			if !ok {
				return filepath.SkipDir
			}
			liveDirs = append(liveDirs, normalized)
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return nil
		}
		normalized, ok := m.managedRelativePath(path)
		if !ok {
			return nil
		}
		liveFiles[normalized] = path
		return nil
	}); err != nil {
		return err
	}

	// Remove shadow-worktree files that disappeared from the live tree before
	// copying current contents. `git add -A` can only stage a deletion after
	// the stale mirror file itself is gone.
	tracked := exec.Command("git", "ls-files", "-z")
	tracked.Dir = m.shadowDir
	trackedPaths, err := tracked.Output()
	if err != nil {
		return fmt.Errorf("checkpoint: enumerate shadow files: %w", err)
	}
	for _, rel := range strings.Split(string(trackedPaths), "\x00") {
		if rel == "" {
			continue
		}
		normalized, ok := m.validRelativePath(rel)
		if !ok || normalized != filepath.ToSlash(rel) {
			return fmt.Errorf("checkpoint: unsafe tracked shadow path %q", rel)
		}
		if _, exists := liveFiles[normalized]; !exists {
			if removeErr := os.RemoveAll(filepath.Join(m.shadowDir, filepath.FromSlash(normalized))); removeErr != nil {
				return fmt.Errorf("checkpoint: remove stale shadow path %s: %w", normalized, removeErr)
			}
		}
	}

	// Resolve file→directory conflicts in parent-before-child order.
	for _, rel := range liveDirs {
		dst := filepath.Join(m.shadowDir, filepath.FromSlash(rel))
		if info, err := os.Lstat(dst); err == nil && !info.IsDir() {
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("checkpoint: replace shadow file with directory %s: %w", rel, err)
			}
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("checkpoint: inspect shadow directory %s: %w", rel, err)
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return fmt.Errorf("checkpoint: create shadow directory %s: %w", rel, err)
		}
	}

	for rel, path := range liveFiles {
		dst := filepath.Join(m.shadowDir, filepath.FromSlash(rel))
		// Resolve directory→file (and any non-regular) conflicts explicitly.
		if info, err := os.Lstat(dst); err == nil && !info.Mode().IsRegular() {
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("checkpoint: replace shadow path with file %s: %w", rel, err)
			}
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("checkpoint: inspect shadow file %s: %w", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("checkpoint: create parent for %s: %w", rel, err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("checkpoint: read %s: %w", rel, err)
		}
		if err := os.WriteFile(dst, body, 0o600); err != nil {
			return fmt.Errorf("checkpoint: write %s: %w", rel, err)
		}
		mode := os.FileMode(0o600)
		if info, statErr := os.Stat(path); statErr == nil && info.Mode()&0o111 != 0 {
			mode = 0o700
		} else if statErr != nil {
			return fmt.Errorf("checkpoint: inspect mode %s: %w", rel, statErr)
		}
		if err := os.Chmod(dst, mode); err != nil {
			return fmt.Errorf("checkpoint: chmod %s: %w", rel, err)
		}
	}
	return nil
}

const managedPathsFilename = "metis-managed-paths.json"

var ErrManagedPathChanged = errors.New("checkpoint: managed path changed after tool execution")

// RecordManagedPath marks a file successfully created or edited by a direct
// file tool. The union is deliberately narrower than "whatever currently
// exists in cwd": Restore may remove a later-created managed file, but must
// never infer that an unrelated user file is safe to delete.
func (m *Manager) RecordManagedPath(path string) error {
	return m.RecordManagedPaths([]string{path})
}

// RecordManagedPaths atomically extends the explicit restore allow-list.
// Paths may be absolute or cwd-relative; symlink escapes, skipped trees,
// non-regular payloads, and oversized files are ignored.
func (m *Manager) RecordManagedPaths(paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return err
	}
	managed, err := m.loadManagedPathStates()
	if err != nil {
		return err
	}
	changed := false
	for _, path := range paths {
		rel, ok := m.managedRelativePath(path)
		if !ok {
			continue
		}
		state, stateErr := m.pathState(rel)
		if stateErr != nil {
			continue
		}
		if previous, exists := managed[rel]; !exists || previous != state {
			managed[rel] = state
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return m.writeManagedPathStates(managed)
}

// CapturePathStates resolves tool-reported paths to safe cwd-relative names
// and fingerprints their current post-tool state. Restore later refuses to
// overwrite/delete a path whose state changed independently in the meantime.
func (m *Manager) CapturePathStates(paths []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return nil, errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return nil, err
	}
	states := make(map[string]string)
	for _, path := range paths {
		rel, ok := m.managedRelativePath(path)
		if !ok {
			continue
		}
		state, err := m.pathState(rel)
		if err != nil {
			continue
		}
		states[rel] = state
	}
	return states, nil
}

func (m *Manager) pathState(rel string) (string, error) {
	path := filepath.Join(m.cwd, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("checkpoint: symlink path is unmanaged")
	}
	if info.IsDir() {
		return "dir", nil
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return "", errors.New("checkpoint: unsupported managed path type or size")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	execBit := "0"
	if info.Mode()&0o111 != 0 {
		execBit = "x"
	}
	return execBit + ":" + hex.EncodeToString(sum[:]), nil
}

func (m *Manager) managedRelativePath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.cwd, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	absCWD, err := filepath.Abs(m.cwd)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absCWD, absPath)
	if err != nil {
		return "", false
	}
	normalized, ok := m.validRelativePath(rel)
	if !ok {
		return "", false
	}
	// Lexical containment is insufficient: cwd/link/file can point outside the
	// project. Reject every existing symlink component. Missing descendants are
	// safe to record because their first existing parent was checked.
	current := absCWD
	components := strings.Split(filepath.FromSlash(normalized), string(os.PathSeparator))
	for _, component := range components {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) || errors.Is(statErr, syscall.ENOTDIR) {
			break
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return normalized, true
}

func (m *Manager) validRelativePath(rel string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", false
	}
	for _, component := range strings.Split(clean, string(os.PathSeparator)) {
		if component == "" || component == "." || component == ".." || skipDirs[component] {
			return "", false
		}
	}
	return filepath.ToSlash(clean), true
}

func (m *Manager) managedPathsFile() string {
	return filepath.Join(m.shadowDir, ".git", managedPathsFilename)
}

func (m *Manager) loadManagedPathStates() (map[string]string, error) {
	paths := make(map[string]string)
	body, err := os.ReadFile(m.managedPathsFile())
	if err != nil {
		if os.IsNotExist(err) {
			return paths, nil
		}
		return nil, err
	}
	var saved map[string]string
	if err := json.Unmarshal(body, &saved); err != nil {
		return nil, fmt.Errorf("checkpoint: read managed paths: %w", err)
	}
	for rel, state := range saved {
		if normalized, ok := m.validRelativePath(rel); ok && normalized == rel {
			paths[rel] = state
		}
	}
	return paths, nil
}

func (m *Manager) recordManagedRelativePath(rel string) error {
	paths, err := m.loadManagedPathStates()
	if err != nil {
		return err
	}
	state, err := m.pathState(rel)
	if err != nil {
		return err
	}
	if previous, exists := paths[rel]; exists && previous == state {
		return nil
	}
	paths[rel] = state
	return m.writeManagedPathStates(paths)
}

func (m *Manager) writeManagedPathStates(paths map[string]string) error {
	body, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.managedPathsFile(), body, 0o600); err != nil {
		return fmt.Errorf("checkpoint: write managed paths: %w", err)
	}
	return nil
}

func (m *Manager) treePaths(ref string) (map[string]struct{}, error) {
	out, err := m.gitOutputBytes("ls-tree", "-r", "--name-only", "-z", ref)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	for _, raw := range strings.Split(string(out), "\x00") {
		if raw == "" {
			continue
		}
		rel := filepath.ToSlash(raw)
		if normalized, ok := m.validRelativePath(rel); ok && normalized == rel {
			paths[rel] = struct{}{}
		}
	}
	return paths, nil
}

// ChangedPaths returns the bounded, non-skipped regular-file delta between a
// pre-tool checkpoint and the live tree. It is used after successful Bash
// (and other mutating tools) to attribute creates, edits, deletes and renames
// without granting restore permission to unrelated snapshot contents.
func (m *Manager) ChangedPaths(hash string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return nil, errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return nil, err
	}
	target, err := m.treePaths(hash)
	if err != nil {
		return nil, err
	}
	live := make(map[string]string)
	err = filepath.Walk(m.cwd, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("checkpoint: inspect live delta %s: %w", path, walkErr)
		}
		if info == nil {
			return fmt.Errorf("checkpoint: inspect live delta %s: missing file info", path)
		}
		if info.IsDir() {
			if path != m.cwd && skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			if m.shadowDir != "" && (path == m.shadowDir || strings.HasPrefix(path, m.shadowDir+string(os.PathSeparator))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return nil
		}
		rel, relErr := filepath.Rel(m.cwd, path)
		if relErr != nil {
			return relErr
		}
		if normalized, ok := m.validRelativePath(rel); ok {
			live[normalized] = path
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	candidates := make(map[string]struct{}, len(target)+len(live))
	for rel := range target {
		candidates[rel] = struct{}{}
	}
	for rel := range live {
		candidates[rel] = struct{}{}
	}
	changed := make([]string, 0)
	for rel := range candidates {
		_, inTarget := target[rel]
		livePath, inLive := live[rel]
		if inTarget != inLive {
			changed = append(changed, rel)
			continue
		}
		if !inTarget {
			continue
		}
		before, blobErr := m.gitOutputBytes("show", hash+":"+rel)
		if blobErr != nil {
			return nil, blobErr
		}
		after, readErr := os.ReadFile(livePath)
		if readErr != nil {
			return nil, fmt.Errorf("checkpoint: read changed path %s: %w", rel, readErr)
		}
		if !bytes.Equal(before, after) {
			changed = append(changed, rel)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// Checkpoint is one entry in the shadow log.
type Checkpoint struct {
	Hash    string
	Time    time.Time
	Tool    string
	ArgsKey string
	Message string
}

// List returns the most-recent N checkpoints. n <= 0 returns all.
func (m *Manager) List(n int) ([]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return nil, nil
	}
	if err := m.initShadowRepo(); err != nil {
		return nil, err
	}
	args := []string{"log", "--format=%H%x09%s"}
	if n > 0 {
		args = append(args, fmt.Sprintf("-n%d", n))
	}
	out, err := m.gitOutput(args...)
	if err != nil {
		return nil, nil // empty repo → no checkpoints
	}
	var cps []Checkpoint
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		cps = append(cps, parseCheckpoint(parts[0], parts[1]))
	}
	return cps, nil
}

// parseCheckpoint splits the commit subject (built by Snap) back
// into structured fields. Robust against malformed entries: returns
// a Checkpoint with whatever it could recover.
func parseCheckpoint(hash, subject string) Checkpoint {
	cp := Checkpoint{Hash: hash, Message: subject}
	parts := strings.SplitN(subject, "|", 4)
	if len(parts) >= 1 {
		if t, err := time.Parse(time.RFC3339, parts[0]); err == nil {
			cp.Time = t
		}
	}
	if len(parts) >= 2 {
		cp.Tool = parts[1]
	}
	if len(parts) >= 3 {
		cp.ArgsKey = parts[2]
	}
	if len(parts) >= 4 {
		cp.Message = parts[3]
	}
	return cp
}

// Restore restores every explicitly managed path. New rewind callers should
// prefer RestorePaths so per-turn provenance can narrow the scope further.
func (m *Manager) Restore(hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return err
	}
	managed, err := m.loadManagedPathStates()
	if err != nil {
		return err
	}
	return m.restorePathStatesLocked(hash, managed)
}

// RestorePaths materializes only the explicitly attributed paths. It never
// checks out the whole shadow tree, so an unrelated file that merely existed
// in a snapshot is not overwritten by a code rewind.
func (m *Manager) RestorePaths(hash string, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return err
	}
	states := make(map[string]string)
	for _, path := range paths {
		rel, ok := m.managedRelativePath(path)
		if !ok {
			continue
		}
		state, err := m.pathState(rel)
		if err != nil {
			continue
		}
		states[rel] = state
	}
	return m.restorePathStatesLocked(hash, states)
}

// RestorePathStates restores only paths whose current state still matches the
// post-tool fingerprint captured by CapturePathStates. It provides an
// optimistic ownership check: if the user edits, deletes, or recreates a path
// after Metis touched it, rewind fails safely before any file mutation.
func (m *Manager) RestorePathStates(hash string, states map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return err
	}
	return m.restorePathStatesLocked(hash, states)
}

type restoreFile struct {
	rel  string
	body []byte
	mode os.FileMode
}

type restoreBackup struct {
	rel    string
	exists bool
	isDir  bool
	body   []byte
	mode   os.FileMode
	files  []restoreBackupFile
}

type restoreBackupFile struct {
	rel  string
	body []byte
	mode os.FileMode
}

func (m *Manager) restorePathStatesLocked(hash string, states map[string]string) error {
	targetPaths, err := m.treePaths(hash)
	if err != nil {
		return fmt.Errorf("checkpoint restore: %w", err)
	}
	unique := make(map[string]string, len(states))
	for path, expected := range states {
		rel, ok := m.managedRelativePath(path)
		if !ok {
			return fmt.Errorf("checkpoint restore: unsafe managed path %q", path)
		}
		current, stateErr := m.pathState(rel)
		if stateErr != nil || current != expected {
			return fmt.Errorf("%w: %s", ErrManagedPathChanged, rel)
		}
		unique[rel] = expected
	}
	// A managed path that is currently a directory may be removed to restore a
	// historical file or absence. Refuse unless every regular descendant is
	// explicitly attributed with a matching fingerprint. Any extra user file,
	// symlink, device, skipped/oversized object, or changed descendant aborts
	// before the journal or working tree is touched.
	for rel, expected := range unique {
		if expected != "dir" {
			continue
		}
		root := filepath.Join(m.cwd, filepath.FromSlash(rel))
		if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root {
				return nil
			}
			childRel, err := filepath.Rel(m.cwd, path)
			if err != nil {
				return err
			}
			childRel = filepath.ToSlash(childRel)
			if info.IsDir() {
				if skipDirs[info.Name()] {
					return fmt.Errorf("%w: unmanaged directory %s", ErrManagedPathChanged, childRel)
				}
				return nil
			}
			if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
				return fmt.Errorf("%w: unsupported descendant %s", ErrManagedPathChanged, childRel)
			}
			want, managed := unique[childRel]
			if !managed {
				return fmt.Errorf("%w: unmanaged descendant %s", ErrManagedPathChanged, childRel)
			}
			got, err := m.pathState(childRel)
			if err != nil || got != want {
				return fmt.Errorf("%w: changed descendant %s", ErrManagedPathChanged, childRel)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// Journal every top-level attributed path before mutation. If a later
	// write/delete fails, restore these backups in reverse order so a multi-file
	// rewind does not leave a half-applied working tree.
	topLevel := make([]string, 0, len(unique))
	for rel := range unique {
		hasManagedAncestor := false
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			if _, ok := unique[parent]; ok {
				hasManagedAncestor = true
				break
			}
		}
		if !hasManagedAncestor {
			topLevel = append(topLevel, rel)
		}
	}
	backups := make([]restoreBackup, 0, len(topLevel))
	for _, rel := range topLevel {
		backup, backupErr := m.captureBackup(rel)
		if backupErr != nil {
			return backupErr
		}
		backups = append(backups, backup)
	}
	rollback := func(cause error) error {
		var rollbackErr error
		for index := len(backups) - 1; index >= 0; index-- {
			if err := m.restoreBackup(backups[index]); err != nil {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("checkpoint restore rollback: %w", rollbackErr))
		}
		return cause
	}

	files := make([]restoreFile, 0, len(unique))
	for rel := range unique {
		if _, present := targetPaths[rel]; present {
			body, blobErr := m.gitOutputBytes("show", hash+":"+rel)
			if blobErr != nil {
				return fmt.Errorf("checkpoint restore: read %s: %w", rel, blobErr)
			}
			mode := os.FileMode(0o600)
			listing, listErr := m.gitOutputBytes("ls-tree", "-z", hash, "--", rel)
			if listErr != nil {
				return fmt.Errorf("checkpoint restore: mode %s: %w", rel, listErr)
			}
			if bytes.HasPrefix(listing, []byte("100755 ")) {
				mode = 0o700
			}
			// Preflight path-type conflicts. Replacing an ancestor is allowed only
			// when that ancestor is itself in this attributed restore set.
			parentRel := filepath.Dir(filepath.FromSlash(rel))
			for parentRel != "." {
				if info, statErr := os.Lstat(filepath.Join(m.cwd, parentRel)); statErr == nil && !info.IsDir() {
					parentSlash := filepath.ToSlash(parentRel)
					if _, authorized := unique[parentSlash]; !authorized {
						return fmt.Errorf("checkpoint restore: unmanaged parent conflict %s", parentSlash)
					}
				} else if statErr != nil && !os.IsNotExist(statErr) {
					return fmt.Errorf("checkpoint restore: inspect parent %s: %w", parentRel, statErr)
				}
				parentRel = filepath.Dir(parentRel)
			}
			files = append(files, restoreFile{rel: rel, body: body, mode: mode})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.Count(files[i].rel, "/") < strings.Count(files[j].rel, "/")
	})
	for _, file := range files {
		path := filepath.Join(m.cwd, filepath.FromSlash(file.rel))
		if info, statErr := os.Lstat(path); statErr == nil && !info.Mode().IsRegular() {
			if err := os.RemoveAll(path); err != nil {
				return rollback(fmt.Errorf("checkpoint restore: replace %s: %w", file.rel, err))
			}
		}
		parent := filepath.Dir(path)
		for parent != m.cwd {
			if info, statErr := os.Lstat(parent); statErr == nil && !info.IsDir() {
				if err := os.RemoveAll(parent); err != nil {
					return rollback(fmt.Errorf("checkpoint restore: replace parent %s: %w", parent, err))
				}
			}
			parent = filepath.Dir(parent)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return rollback(fmt.Errorf("checkpoint restore: mkdir for %s: %w", file.rel, err))
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".metis-rewind-*")
		if err != nil {
			return rollback(fmt.Errorf("checkpoint restore: temp for %s: %w", file.rel, err))
		}
		tmpName := tmp.Name()
		if _, err = tmp.Write(file.body); err == nil {
			err = tmp.Chmod(file.mode)
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmpName, path)
		}
		if err != nil {
			_ = os.Remove(tmpName)
			return rollback(fmt.Errorf("checkpoint restore: write %s: %w", file.rel, err))
		}
	}

	deletions := make([]string, 0)
	for rel := range unique {
		if _, present := targetPaths[rel]; present {
			continue
		}
		isTargetDir := false
		for target := range targetPaths {
			if strings.HasPrefix(target, rel+"/") {
				isTargetDir = true
				break
			}
		}
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))); parent != "." && !isTargetDir; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			if _, targetFile := targetPaths[parent]; targetFile {
				isTargetDir = true
			}
		}
		if !isTargetDir {
			deletions = append(deletions, rel)
		}
	}
	sort.Slice(deletions, func(i, j int) bool {
		return strings.Count(deletions[i], "/") > strings.Count(deletions[j], "/")
	})
	for _, rel := range deletions {
		path := filepath.Join(m.cwd, filepath.FromSlash(rel))
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			return rollback(fmt.Errorf("checkpoint restore: remove managed path %s: %w", rel, err))
		}
	}
	return nil
}

func (m *Manager) captureBackup(rel string) (restoreBackup, error) {
	backup := restoreBackup{rel: rel}
	path := filepath.Join(m.cwd, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return backup, nil
	}
	if err != nil {
		return backup, fmt.Errorf("checkpoint restore: backup %s: %w", rel, err)
	}
	backup.exists = true
	backup.mode = info.Mode().Perm()
	if info.IsDir() {
		backup.isDir = true
		err := filepath.Walk(path, func(child string, childInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if child == path || childInfo.IsDir() {
				return nil
			}
			if !childInfo.Mode().IsRegular() || childInfo.Size() > maxFileBytes {
				return fmt.Errorf("checkpoint restore: unsupported backup descendant %s", child)
			}
			body, err := os.ReadFile(child)
			if err != nil {
				return err
			}
			childRel, err := filepath.Rel(path, child)
			if err != nil {
				return err
			}
			backup.files = append(backup.files, restoreBackupFile{rel: childRel, body: body, mode: childInfo.Mode().Perm()})
			return nil
		})
		if err != nil {
			return backup, err
		}
		return backup, nil
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return backup, fmt.Errorf("checkpoint restore: backup unsupported path %s", rel)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return backup, err
	}
	backup.body = body
	return backup, nil
}

func (m *Manager) restoreBackup(backup restoreBackup) error {
	path := filepath.Join(m.cwd, filepath.FromSlash(backup.rel))
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if !backup.exists {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if backup.isDir {
		if err := os.MkdirAll(path, backup.mode); err != nil {
			return err
		}
		for _, file := range backup.files {
			target := filepath.Join(path, file.rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(target, file.body, file.mode); err != nil {
				return err
			}
		}
		return nil
	}
	return os.WriteFile(path, backup.body, backup.mode)
}

// HashArgs returns a short hex digest of any tool-input map. Used as
// the args-key in the checkpoint metadata so /rollback can show
// "edited file X" without unpacking the full args.
func HashArgs(args map[string]any) string {
	if len(args) == 0 {
		return "noargs"
	}
	// Stable serialization — go map iteration is random.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// no `sort` import — manual two-line sort
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%v\n", k, args[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}
