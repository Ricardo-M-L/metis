package checkpoint

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

// lockContext does not abandon a goroutine blocked in Mutex.Lock. Legacy
// callers keep the same mutex and semantics; cancellable callers can leave a
// contended checkpoint without racing its current owner.
func (m *Manager) lockContext(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if m.mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// The caller owns m.mu. Cancellation is retryable, unlike an initialization
// failure: pressing Esc must not permanently consume a sync.Once latch.
func (m *Manager) initShadowRepoContext(ctx context.Context) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if reason := m.disabledReasonLocked(); reason != nil {
		return reason
	}
	if m.initialized {
		return nil
	}
	err := os.MkdirAll(m.shadowDir, 0o700)
	if err == nil {
		// Existence of .git is not proof of completed initialization. Re-running
		// git init is idempotent and repairs an interrupted partial directory.
		_, err = m.gitOutputBytesContext(ctx, "init", "--quiet")
	}
	// Repeat these after an interrupted initialization as well: git init may
	// have finished just before cancellation, without an identity being set.
	if err == nil {
		_, err = m.gitOutputBytesContext(ctx, "config", "user.email", "metis@local")
	}
	if err == nil {
		_, err = m.gitOutputBytesContext(ctx, "config", "user.name", "metis")
	}
	// The shadow is a byte-for-byte checkpoint store, not another checkout of
	// the user's repository. Do not let copied .gitattributes, global Git
	// settings, or their clean filters normalize/execute while taking a snapshot.
	// info/attributes has higher precedence than every copied attributes file.
	if err == nil {
		_, err = m.gitOutputBytesContext(ctx, "config", "core.autocrlf", "false")
	}
	if err == nil {
		var saved []byte
		saved, err = os.ReadFile(m.rawSnapshotFormatPath())
		if os.IsNotExist(err) {
			err = nil
		}
		m.rawFormatMarked = string(saved) == rawSnapshotFormat
		if err == nil && !m.rawFormatMarked {
			_, indexErr := os.Stat(filepath.Join(m.shadowDir, ".git", "index"))
			m.rawIndexMigration = indexErr == nil
			if indexErr != nil && !os.IsNotExist(indexErr) {
				err = indexErr
			}
		}
	}
	if err == nil {
		infoDir := filepath.Join(m.shadowDir, ".git", "info")
		err = os.MkdirAll(infoDir, 0o700)
		if err == nil {
			err = os.WriteFile(filepath.Join(infoDir, "attributes"), []byte("* -text -eol -ident -filter -working-tree-encoding\n"), 0o600)
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err != nil {
		m.initErr = err
		m.disabled = true
		return m.disabledReasonLocked()
	}
	m.initialized = true
	return nil
}

func (m *Manager) gitOutputBytesContext(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = m.shadowDir
	// Git filters/hooks can spawn descendants. The isolated process group and
	// bounded pipe join prevent them from retaining a cancelled scan forever.
	jobs.ApplyProcessGroup(cmd)
	cmd.Cancel = func() error {
		// Git's signal handler removes its own index/config/ref lockfiles.
		// Immediate SIGKILL strands them and poisons every subsequent snapshot.
		// Signal the Git leader first, then synchronously stop its entire owned
		// group after a short cleanup grace (including filters/hooks). No detached
		// killer or user-owned lockfile deletion is needed.
		if err := cmd.Process.Signal(os.Interrupt); err == nil {
			timer := time.NewTimer(100 * time.Millisecond)
			<-timer.C
		}
		jobs.KillProcessGroup(cmd.Process)
		return nil
	}
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// SnapContext bounds the complete pre-tool operation, not only the shell
// command that follows it. Failed copies are never committed; the next copy
// reconciles both tracked and untracked mirror leftovers before staging.
func (m *Manager) SnapContext(ctx context.Context, toolName, argsHash, message string) (string, error) {
	return m.snapshotContext(ctx, toolName, argsHash, message, false)
}

// BaselineContext validates the current working tree even when no bytes have
// changed, and returns the exact verified HEAD in that case. A caller must not
// attach an old turn's baseline merely because SnapContext returned no new commit.
func (m *Manager) BaselineContext(ctx context.Context, toolName, argsHash, message string) (string, error) {
	return m.snapshotContext(ctx, toolName, argsHash, message, true)
}

func (m *Manager) snapshotContext(ctx context.Context, toolName, argsHash, message string, baseline bool) (hash string, err error) {
	observer := newScanObserver(ctx)
	observer.stage("lock")
	if err = m.lockContext(ctx); err != nil {
		_, err = observer.finish(err)
		return "", err
	}
	defer m.mu.Unlock()
	defer func() { m.lastScan, err = observer.finish(err) }()
	if m.automaticOff.Load() && !m.disabled {
		return "", ErrDisabledByUser
	}
	if m.disabled {
		return "", m.disabledReasonLocked()
	}
	observer.stage("init")
	if err := m.initShadowRepoContext(ctx); err != nil {
		return "", err
	}
	if err := m.syncTreeContext(ctx, observer); err != nil {
		return "", err
	}
	observer.stage("stage")
	// Inventory already applied the explicit checkpoint scope. A copied
	// .gitignore must not silently omit existing files from the baseline and
	// later make a readonly command appear to have created those files.
	if _, err := m.gitOutputBytesContext(ctx, "add", "-f", "-A"); err != nil {
		return "", err
	}
	if m.rawIndexMigration {
		// A legacy index can cache a normalized blob even when the mirror's
		// current bytes/mtime are unchanged. Re-clean once under the raw-byte
		// attributes policy. Historical commits remain untouched.
		observer.stage("migrate-index")
		if _, err := m.gitOutputBytesContext(ctx, "add", "--renormalize", "."); err != nil {
			return "", err
		}
	}
	if !m.rawFormatMarked {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		if err := os.WriteFile(m.rawSnapshotFormatPath(), []byte(rawSnapshotFormat), 0o600); err != nil {
			return "", err
		}
		m.rawFormatMarked = true
		m.rawIndexMigration = false
	}
	head, headErr := m.gitOutputBytesContext(ctx, "rev-parse", "--verify", "HEAD")
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	firstCommit := headErr != nil
	changed, err := m.gitOutputBytesContext(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return "", err
	}
	if len(changed) == 0 && !firstCommit {
		m.hasBaseline.Store(true)
		if baseline {
			return strings.TrimSpace(string(head)), nil
		}
		return "", nil
	}
	observer.stage("commit")
	commitMsg := fmt.Sprintf("%s|%s|%s|%s", time.Now().UTC().Format(time.RFC3339), toolName, argsHash, message)
	args := []string{"commit", "-q", "-m", commitMsg}
	if firstCommit {
		args = append(args, "--allow-empty")
	}
	if _, err := m.gitOutputBytesContext(ctx, args...); err != nil {
		return "", err
	}
	out, err := m.gitOutputBytesContext(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	m.hasBaseline.Store(true)
	return strings.TrimSpace(string(out)), nil
}

const rawSnapshotFormat = "raw-bytes-v1\n"

func (m *Manager) rawSnapshotFormatPath() string {
	return filepath.Join(m.shadowDir, ".git", "metis-raw-snapshot-format")
}

// treeBlobIDs obtains every baseline object ID in one process. NUL records
// and the first TAB separator preserve filenames containing tabs/newlines.
type treeBlob struct {
	objectID   string
	executable bool
}

func (m *Manager) treeBlobIDs(ctx context.Context, ref string) (map[string]treeBlob, error) {
	out, err := m.gitOutputBytesContext(ctx, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	blobs := make(map[string]treeBlob)
	for _, record := range strings.Split(string(out), "\x00") {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (len(fields[2]) != 40 && len(fields[2]) != 64) {
			return nil, errors.New("checkpoint: malformed snapshot tree entry")
		}
		rel := filepath.ToSlash(path)
		if normalized, valid := m.validRelativePath(rel); valid && normalized == rel {
			blobs[rel] = treeBlob{objectID: fields[2], executable: fields[0] == "100755"}
		}
	}
	return blobs, nil
}

func gitBlobID(body []byte, oidLength int) string {
	var digest hash.Hash = sha1.New()
	if oidLength == 64 {
		digest = sha256.New()
	}
	_, _ = fmt.Fprintf(digest, "blob %d\x00", len(body))
	_, _ = digest.Write(body)
	return fmt.Sprintf("%x", digest.Sum(nil))
}

// ChangedPathsContext keeps the existing content-based attribution and
// file/skip limits, without per-file Git processes. Raw live blob hashes match
// the old `git show` versus live bytes comparison (no clean filters invoked).
func (m *Manager) ChangedPathsContext(ctx context.Context, hash string) (changed []string, err error) {
	observer := newScanObserver(ctx)
	observer.stage("lock")
	if err = m.lockContext(ctx); err != nil {
		_, err = observer.finish(err)
		return nil, err
	}
	defer m.mu.Unlock()
	defer func() { m.lastScan, err = observer.finish(err) }()
	if m.disabled {
		return nil, m.disabledReasonLocked()
	}
	observer.stage("init")
	if err := m.initShadowRepoContext(ctx); err != nil {
		return nil, err
	}
	observer.stage("baseline")
	target, err := m.treeBlobIDs(ctx, hash)
	if err != nil {
		return nil, err
	}
	source, err := os.OpenRoot(m.cwd)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	tree, err := m.inventoryTree(ctx, source, observer)
	if err != nil {
		return nil, err
	}
	observer.stage("compare")
	changed, err = processLiveFiles(ctx, tree.files, observer, func(file liveFile) fileResult {
		before, exists := target[file.rel]
		if !exists {
			return fileResult{rel: file.rel, changed: true}
		}
		after, err := readLiveFile(source, file)
		if err != nil {
			return fileResult{err: err}
		}
		different := gitBlobID(after, len(before.objectID)) != before.objectID ||
			(file.info.Mode()&0o111 != 0) != before.executable
		return fileResult{rel: file.rel, changed: different, unchanged: !different}
	})
	if err != nil {
		return nil, err
	}
	for _, file := range tree.files {
		delete(target, file.rel)
	}
	for rel := range target {
		changed = append(changed, rel)
		observer.stats.RemovedFiles++
	}
	sort.Strings(changed)
	return changed, nil
}
