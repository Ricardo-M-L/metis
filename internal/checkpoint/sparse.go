package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const sparsePathsFilename = "metis-sparse-paths.json"
const maxSparsePaths = 8192

// Sparse is target-only protection for a broad workspace (automatic at home).
// Its snapshots
// contain only targets registered before direct file edits, never the home tree.
func (m *Manager) Sparse() bool { return m.sparse.Load() }

type sparsePathSet struct {
	Workspace string   `json:"workspace"`
	Paths     []string `json:"paths"`
}

func (m *Manager) sparseWorkspace() string {
	if cwd, err := filepath.EvalSymlinks(m.cwd); err == nil {
		return cwd
	}
	return filepath.Clean(m.cwd)
}

func (m *Manager) readSparseSet(path string, missingOK bool) (map[string]bool, error) {
	result := make(map[string]bool)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && missingOK {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return nil, errors.New("checkpoint: invalid session file registry")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var saved sparsePathSet
	if err = json.Unmarshal(body, &saved); err != nil {
		return nil, err
	}
	if saved.Workspace != m.sparseWorkspace() || len(saved.Paths) > maxSparsePaths {
		return nil, errors.New("checkpoint: session file registry identity mismatch")
	}
	for _, path := range saved.Paths {
		rel, ok := safeRelativePath(path)
		if !ok || rel != path {
			return nil, errors.New("checkpoint: unsafe session file registry path")
		}
		result[rel] = true
	}
	return result, nil
}

func (m *Manager) loadSparsePathsLocked() error {
	paths, err := m.readSparseSet(filepath.Join(m.shadowDir, ".git", sparsePathsFilename), true)
	if err == nil {
		m.sparsePaths = paths
	}
	return err
}

func (m *Manager) writeSparseSet(path string, paths map[string]bool) error {
	if len(paths) > maxSparsePaths {
		return errors.New("checkpoint: session file limit reached")
	}
	saved := sparsePathSet{Workspace: m.sparseWorkspace(), Paths: sortedSparsePaths(paths)}
	body, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".sparse-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func sortedSparsePaths(paths map[string]bool) []string {
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func (m *Manager) sparseBaselineFile(hash string) string {
	return filepath.Join(m.shadowDir, ".git", "metis-sparse-baselines", hash+".json")
}

func (m *Manager) readSparseBaselineLocked(ctx context.Context, hash string) (map[string]bool, error) {
	captured, err := m.readSparseSet(m.sparseBaselineFile(hash), false)
	if err != nil {
		return nil, err
	}
	body, err := m.gitOutputBytesContext(ctx, "show", "-s", "--format=%B", hash)
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(sortedSparsePaths(captured))
	sum := sha256.Sum256(identity)
	expected := fmt.Sprintf("session-files=%x", sum)
	if !strings.Contains(string(body), "\n"+expected+"\n") {
		return nil, errors.New("checkpoint: session baseline captured-set identity mismatch")
	}
	return captured, nil
}

// BaselinePathsContext saves preimages for a direct file tool. A priorBaseline
// extends the same turn's point without replacing its earlier preimages. The
// saved path set includes absent targets, so a repeated edit of a newly created
// file cannot silently turn its post-create bytes into the original state.
func (m *Manager) BaselinePathsContext(ctx context.Context, paths []string, priorBaseline, toolName, argsHash, message string) (string, error) {
	hashes, err := m.ExtendBaselinePathsContext(ctx, paths, []string{priorBaseline}, toolName, argsHash, message)
	if err != nil {
		return "", err
	}
	return hashes[0], nil
}

// ExtendBaselinePathsContext captures the live target preimages once and uses
// those exact Git blobs for every historical point that has not seen a target.
// This lets a later first edit be safely rewound across earlier session turns.
func (m *Manager) ExtendBaselinePathsContext(ctx context.Context, paths []string, priorBaselines []string, toolName, argsHash, message string) (hashes []string, err error) {
	observer := newScanObserver(ctx)
	if err = m.lockContext(ctx); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	defer func() { m.lastScan, err = observer.finish(err) }()
	if !m.Sparse() {
		return nil, errors.New("checkpoint: session file baseline requires a broad workspace")
	}
	if m.automaticOff.Load() && !m.disabled {
		return nil, ErrDisabledByUser
	}
	if m.disabled {
		return nil, m.disabledReasonLocked()
	}
	if len(paths) == 0 || len(priorBaselines) == 0 {
		return nil, errors.New("checkpoint: file target and baseline are required")
	}
	capturedSets := make([]map[string]bool, len(priorBaselines))
	for i, prior := range priorBaselines {
		if prior == "" {
			continue
		}
		if !validObjectID(prior) {
			return nil, ErrSnapshotMissing
		}
		capturedSets[i], err = m.readSparseBaselineLocked(ctx, prior)
		if err != nil {
			return nil, err
		}
	}
	targets := make(map[string]bool)
	for _, path := range paths {
		rel, ok := m.relativePath(path, false)
		if !ok {
			return nil, errors.New("checkpoint: file target is outside the session workspace or unsupported")
		}
		info, stateErr := os.Lstat(filepath.Join(m.cwd, filepath.FromSlash(rel)))
		if stateErr != nil && !os.IsNotExist(stateErr) {
			return nil, stateErr
		}
		if stateErr == nil && (!info.Mode().IsRegular() || info.Size() > maxFileBytes) {
			return nil, errors.New("checkpoint: file target must be a supported regular file")
		}
		targets[rel] = true
	}
	if err = m.initShadowRepoContext(ctx); err != nil {
		return nil, err
	}
	if err = m.loadSparsePathsLocked(); err != nil {
		return nil, err
	}
	next := make(map[string]bool, len(m.sparsePaths)+len(targets))
	for path := range m.sparsePaths {
		next[path] = true
	}
	for path := range targets {
		next[path] = true
	}
	if err = m.writeSparseSet(filepath.Join(m.shadowDir, ".git", sparsePathsFilename), next); err != nil {
		return nil, err
	}
	m.sparsePaths = next
	needSnapshot := false
	for i, prior := range priorBaselines {
		if prior == "" {
			needSnapshot = true
			break
		}
		for path := range targets {
			if !capturedSets[i][path] {
				needSnapshot = true
				break
			}
		}
	}
	if !needSnapshot {
		return append([]string(nil), priorBaselines...), nil
	}
	current, err := m.snapshotLocked(ctx, observer, toolName, argsHash, message, true)
	if err != nil {
		return nil, err
	}
	blobs, err := m.treeBlobIDs(ctx, current)
	if err != nil {
		return nil, err
	}
	hashes = make([]string, len(priorBaselines))
	for i, prior := range priorBaselines {
		captured := capturedSets[i]
		var added []string
		if prior == "" {
			captured = make(map[string]bool, len(next))
			for path := range next {
				captured[path] = true
			}
		} else {
			for path := range targets {
				if !captured[path] {
					added = append(added, path)
					captured[path] = true
				}
			}
			if len(added) == 0 {
				hashes[i] = prior
				continue
			}
		}
		hashes[i], err = m.extendSparseBaselineLocked(ctx, current, prior, added, captured, blobs, toolName, argsHash, message)
		if err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

func (m *Manager) extendSparseBaselineLocked(ctx context.Context, current, prior string, added []string, captured map[string]bool, blobs map[string]treeBlob, toolName, argsHash, message string) (string, error) {
	// The temporary index keeps HEAD and the live mirror unchanged while a
	// historical tree receives only first-seen preimages.
	file, err := os.CreateTemp(filepath.Join(m.shadowDir, ".git"), "sparse-index-*")
	if err != nil {
		return "", err
	}
	index := file.Name()
	if err = file.Close(); err != nil {
		os.Remove(index)
		return "", err
	}
	if err = os.Remove(index); err != nil {
		return "", err
	}
	defer os.Remove(index)
	defer os.Remove(index + ".lock")
	env := append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_LITERAL_PATHSPECS=1")
	git := func(args ...string) ([]byte, error) { return m.gitOutputBytesEnvContext(ctx, env, args...) }
	base := prior
	if base == "" {
		base = current
	}
	if _, err = git("read-tree", base); err != nil {
		return "", err
	}
	sort.Strings(added)
	for _, path := range added {
		if blob, exists := blobs[path]; exists {
			mode := "100644"
			if blob.executable {
				mode = "100755"
			}
			_, err = git("update-index", "--add", "--cacheinfo", mode+","+blob.objectID+","+path)
		} else {
			_, err = git("update-index", "--force-remove", "--", path)
		}
		if err != nil {
			return "", err
		}
	}
	tree, err := git("write-tree")
	if err != nil {
		return "", err
	}
	// The captured set is part of the immutable commit identity, even if adding
	// another absent file leaves the tree bytes unchanged.
	identity, _ := json.Marshal(sortedSparsePaths(captured))
	sum := sha256.Sum256(identity)
	commitMessage := fmt.Sprintf("%s|%s|%s|%s\nsession-files=%x", time.Now().UTC().Format(time.RFC3339Nano), toolName, argsHash, message, sum)
	commit, err := git("commit-tree", strings.TrimSpace(string(tree)), "-p", base, "-m", commitMessage)
	if err != nil {
		return "", err
	}
	hash := strings.TrimSpace(string(commit))
	if !validObjectID(hash) {
		return "", ErrSnapshotMissing
	}
	metadata := m.sparseBaselineFile(hash)
	if old, readErr := m.readSparseSet(metadata, false); readErr == nil {
		if strings.Join(sortedSparsePaths(old), "\x00") != strings.Join(sortedSparsePaths(captured), "\x00") {
			return "", errors.New("checkpoint: immutable session baseline identity conflict")
		}
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	} else if err = m.writeSparseSet(metadata, captured); err != nil {
		return "", err
	}
	if _, err = git("update-ref", "refs/metis/sparse/"+hash, hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (m *Manager) inventorySparseTree(ctx context.Context, root *os.Root, observer *scanObserver) (liveTree, error) {
	var tree liveTree
	directories := make(map[string]bool)
	for _, path := range sortedSparsePaths(m.sparsePaths) {
		if ctx.Err() != nil {
			return tree, context.Cause(ctx)
		}
		// Validate every existing component. No directory enumeration is needed,
		// including the root itself, so unrelated siblings are never visited.
		if _, ok := m.relativePath(path, false); !ok {
			return tree, errors.New("checkpoint: session file target became unsafe")
		}
		info, err := root.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return tree, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return tree, errors.New("checkpoint: session file target became unsupported")
		}
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			directories[parent] = true
		}
		tree.files = append(tree.files, liveFile{rel: path, info: info})
		observer.stats.Files++
		observer.stats.Bytes += info.Size()
		observer.notify(false)
	}
	tree.dirs = sortedSparsePaths(directories)
	observer.stats.Directories += len(tree.dirs)
	return tree, nil
}

func (m *Manager) checkSparseLiveTypesLocked() error {
	for path := range m.sparsePaths {
		if _, ok := m.relativePath(path, false); !ok {
			return errors.New("checkpoint: session file target became unsafe")
		}
		info, err := os.Lstat(filepath.Join(m.cwd, filepath.FromSlash(path)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return errors.New("checkpoint: session file target became unsupported")
		}
	}
	return nil
}
