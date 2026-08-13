package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrSnapshotMissing = errors.New("checkpoint: snapshot repository is unavailable")

// Diff compares two existing shadow-git snapshots. When toHash is empty, it
// compares fromHash with the live working tree and includes files that are
// untracked relative to the shadow index. It never initializes, stages,
// commits, checks out, or otherwise mutates either repository.
func (m *Manager) Diff(fromHash, toHash string) (string, error) {
	return m.DiffContext(context.Background(), fromHash, toHash, 0)
}

// DiffContext is Diff with cancellation and a total output budget. maxBytes
// <= 0 keeps the legacy unlimited behavior for internal callers.
func (m *Manager) DiffContext(ctx context.Context, fromHash, toHash string, maxBytes int) (string, error) {
	if m == nil || !validObjectID(fromHash) || (toHash != "" && !validObjectID(toHash)) {
		return "", ErrSnapshotMissing
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return "", ErrSnapshotMissing
	}
	if info, err := os.Stat(filepath.Join(m.shadowDir, ".git")); err != nil || !info.IsDir() {
		return "", ErrSnapshotMissing
	}

	if toHash != "" {
		return m.readOnlyGitDiffContext(ctx, "", maxBytes, fromHash, toHash)
	}
	tracked, err := m.readOnlyGitDiffContext(ctx, m.cwd, maxBytes, fromHash)
	if err != nil {
		return "", err
	}
	remaining := maxBytes
	if remaining > 0 {
		remaining -= len(tracked)
		if remaining <= 0 {
			return tracked, ErrDiffOutputLimit
		}
	}
	untracked, err := m.untrackedWorkingTreeDiffsContext(ctx, remaining)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDiffOutputLimit) {
			return tracked, err
		}
		// The tracked comparison is still useful if enumeration races a file
		// deletion or the worktree becomes temporarily unreadable.
		return tracked, nil
	}
	return tracked + untracked, nil
}

func (m *Manager) readOnlyGitDiff(workTree string, revisions ...string) (string, error) {
	return m.readOnlyGitDiffContext(context.Background(), workTree, 0, revisions...)
}

var ErrDiffOutputLimit = errors.New("checkpoint: diff output limit exceeded")

type cappedDiffOutput struct {
	buf       bytes.Buffer
	remaining int
	exceeded  bool
}

func (w *cappedDiffOutput) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining <= 0 {
		w.exceeded = true
		return n, nil
	}
	keep := n
	if keep > w.remaining {
		keep = w.remaining
		w.exceeded = true
	}
	_, _ = w.buf.Write(p[:keep])
	w.remaining -= keep
	return n, nil
}

func (m *Manager) readOnlyGitDiffContext(ctx context.Context, workTree string, maxBytes int, revisions ...string) (string, error) {
	args := []string{"--no-pager"}
	if workTree != "" {
		args = append(args, "--work-tree="+workTree)
	}
	args = append(args, "diff", "--no-color", "--no-ext-diff")
	args = append(args, revisions...)
	args = append(args, "--", ".")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = m.shadowDir
	if maxBytes <= 0 {
		out, err := cmd.CombinedOutput()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return string(out), ctxErr
		}
		if err != nil {
			return "", errors.New("checkpoint: read-only git diff failed: " + strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	writer := &cappedDiffOutput{remaining: maxBytes}
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	out := writer.buf.Bytes()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return string(out), ctxErr
	}
	if writer.exceeded {
		return string(out), ErrDiffOutputLimit
	}
	if err != nil {
		return "", errors.New("checkpoint: read-only git diff failed: " + strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (m *Manager) untrackedWorkingTreeDiffs() (string, error) {
	return m.untrackedWorkingTreeDiffsContext(context.Background(), 0)
}

func (m *Manager) untrackedWorkingTreeDiffsContext(ctx context.Context, maxBytes int) (string, error) {
	cmd := exec.CommandContext(ctx,
		"git", "--work-tree="+m.cwd, "ls-files", "--others", "--exclude-standard", "-z",
	)
	cmd.Dir = m.shadowDir
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	var patches strings.Builder
	for _, path := range strings.Split(string(out), "\x00") {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return patches.String(), ctxErr
		}
		if path == "" {
			continue
		}
		absolute := filepath.Join(m.cwd, filepath.FromSlash(path))
		info, err := os.Lstat(absolute)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			continue
		}
		limit := maxBytes
		if limit > 0 {
			limit -= patches.Len()
			if limit <= 0 {
				return patches.String(), ErrDiffOutputLimit
			}
		}
		diff := exec.CommandContext(ctx,
			"git", "--no-pager", "diff", "--no-index", "--no-color", "--no-ext-diff",
			"--", os.DevNull, filepath.FromSlash(path),
		)
		diff.Dir = m.cwd
		var patch []byte
		var diffErr error
		if limit > 0 {
			writer := &cappedDiffOutput{remaining: limit}
			diff.Stdout = writer
			diff.Stderr = writer
			diffErr = diff.Run()
			patch = writer.buf.Bytes()
			if writer.exceeded {
				return patches.String(), ErrDiffOutputLimit
			}
		} else {
			patch, diffErr = diff.CombinedOutput()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return patches.String(), ctxErr
		}
		// git diff --no-index returns 1 when a difference exists.
		if diffErr != nil {
			if exit, ok := diffErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				continue
			}
		}
		patches.Write(patch)
	}
	return patches.String(), nil
}

func validObjectID(hash string) bool {
	if len(hash) < 7 || len(hash) > 64 {
		return false
	}
	for _, char := range hash {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}
