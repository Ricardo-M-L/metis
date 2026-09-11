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
	if m.initialized || m.initErr != nil {
		return m.initErr
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
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err != nil {
		m.initErr = err
		m.disabled = true
		return err
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
	if err := m.lockContext(ctx); err != nil {
		return "", err
	}
	defer m.mu.Unlock()
	if m.disabled {
		return "", errors.New("checkpoint: disabled (prior init error)")
	}
	if err := m.initShadowRepoContext(ctx); err != nil {
		return "", err
	}
	if err := m.copyTreeContext(ctx); err != nil {
		return "", err
	}
	if _, err := m.gitOutputBytesContext(ctx, "add", "-A"); err != nil {
		return "", err
	}
	_, headErr := m.gitOutputBytesContext(ctx, "rev-parse", "--verify", "HEAD")
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	firstCommit := headErr != nil
	changed, err := m.gitOutputBytesContext(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return "", err
	}
	if len(changed) == 0 && !firstCommit {
		return "", nil
	}
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
	return strings.TrimSpace(string(out)), nil
}

// treeBlobIDs obtains every baseline object ID in one process. NUL records
// and the first TAB separator preserve filenames containing tabs/newlines.
func (m *Manager) treeBlobIDs(ctx context.Context, ref string) (map[string]string, error) {
	out, err := m.gitOutputBytesContext(ctx, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return nil, err
	}
	blobs := make(map[string]string)
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
			blobs[rel] = fields[2]
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
func (m *Manager) ChangedPathsContext(ctx context.Context, hash string) ([]string, error) {
	if err := m.lockContext(ctx); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	if m.disabled {
		return nil, errors.New("checkpoint: disabled")
	}
	if err := m.initShadowRepoContext(ctx); err != nil {
		return nil, err
	}
	target, err := m.treeBlobIDs(ctx, hash)
	if err != nil {
		return nil, err
	}
	changed := make([]string, 0)
	err = filepath.Walk(m.cwd, func(path string, info os.FileInfo, walkErr error) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
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
		rel, err := filepath.Rel(m.cwd, path)
		if err != nil {
			return err
		}
		rel, ok := m.validRelativePath(rel)
		if !ok {
			return nil
		}
		before, exists := target[rel]
		delete(target, rel)
		if !exists {
			changed = append(changed, rel)
			return nil
		}
		after, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("checkpoint: read changed path %s: %w", rel, err)
		}
		if gitBlobID(after, len(before)) != before {
			changed = append(changed, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	for rel := range target {
		changed = append(changed, rel)
	}
	sort.Strings(changed)
	return changed, nil
}
