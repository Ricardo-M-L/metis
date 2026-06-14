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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if clean == filepath.Clean(home) {
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
	// `git commit` exits non-zero when there are no changes. Detect
	// that case early via diff-cached so we don't generate an empty
	// commit AND don't return a misleading error.
	if hasChanges, _ := m.gitOutput("diff", "--cached", "--name-only"); hasChanges == "" {
		return "", nil
	}
	commitMsg := fmt.Sprintf("%s|%s|%s|%s",
		time.Now().UTC().Format(time.RFC3339),
		toolName,
		argsHash,
		message,
	)
	if err := m.git("commit", "-q", "-m", commitMsg); err != nil {
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
	return filepath.Walk(m.cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Permission denied / vanished file. Ignore.
			return nil
		}
		rel, err := filepath.Rel(m.cwd, path)
		if err != nil || rel == "." {
			return nil
		}
		// Skip directories we don't track. Returning SkipDir prunes
		// the entire subtree.
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			// Defensive backstop against the self-recursion bug: never
			// descend into the shadow dir we are writing to, even if it
			// somehow lives under cwd under a non-skipped name (e.g. a
			// custom shadowRoot). Belt-and-suspenders with the `.metis`
			// skipDir above.
			if m.shadowDir != "" && (path == m.shadowDir || strings.HasPrefix(path, m.shadowDir+string(os.PathSeparator))) {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip non-regular files (symlinks, devices, sockets).
		if info.Mode()&os.ModeType != 0 {
			return nil
		}
		if info.Size() > maxFileBytes {
			return nil
		}
		dst := filepath.Join(m.shadowDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil
		}
		// Read+write rather than os.Rename — different filesystems may
		// not support hardlinks across mount points.
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// 2026-05-22: surface write errors instead of silently
		// dropping. Earlier this was `_ = os.WriteFile` — a disk-
		// full / permission failure would leave a half-snapshot
		// with no signal to the caller. WalkDir already returns
		// nil on per-entry errors above, so we keep the same
		// "skip this file" semantics but log the write failure
		// for visibility.
		if werr := os.WriteFile(dst, body, 0o600); werr != nil {
			fmt.Fprintf(os.Stderr, "checkpoint: write %s: %v\n", dst, werr)
		}
		return nil
	})
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

// Restore checks out the given checkpoint's tree back into m.cwd.
// hash can be a full sha or a short prefix git understands.
func (m *Manager) Restore(hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepo(); err != nil {
		return err
	}
	// `git checkout-index --prefix=<cwd>/` materialises the entire
	// tree of the given commit into cwd. We use the index of that
	// commit by first temp-checking-out (without affecting working
	// tree) — actually the cleanest path is `git --work-tree=<cwd>
	// checkout <hash> -- .` from inside the shadow repo.
	cmd := exec.Command("git", "--work-tree="+m.cwd, "checkout", hash, "--", ".")
	cmd.Dir = m.shadowDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checkpoint restore: %v: %s", err, string(out))
	}
	return nil
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
