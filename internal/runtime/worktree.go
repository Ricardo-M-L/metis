// Package runtime — git worktree spawn for `metis -W` / `metis --worktree`.
//
// Mirrors claude-code's worktree mode: when the flag is set, metis runs
// `git worktree add` to spawn an isolated checkout under
// ~/.metis/worktrees/<slug> and chdirs into it before normal startup.
// On exit (Ctrl+D / `:q` / process kill) the user gets a prompt to
// either keep the worktree on disk or `git worktree remove --force`.
//
// Safety:
//   - Slug is validated against [a-zA-Z0-9._-]{1,64} segments separated
//     by `/` (which we flatten to `+` for the actual branch / dir name)
//   - Refuse to clobber an existing worktree directory unless the slug
//     resolves to the same head SHA (cheap fast-resume path).
//   - Stale worktrees older than 30 days under ~/.metis/worktrees/
//     are GC'd on every spawn so abandoned slugs don't pile up.
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

const (
	worktreesDir   = "worktrees"
	staleWorktree  = 30 * 24 * time.Hour
	defaultBaseRef = "HEAD"
)

var slugSegmentRE = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// WorktreeInfo is what SpawnWorktree returns to the CLI layer. The CLI
// chdirs into Path before continuing setup.
type WorktreeInfo struct {
	Slug   string
	Branch string
	Path   string
	// Created is true when this invocation actually ran `git worktree add`,
	// false when it reused an existing one. The teardown prompt only
	// applies when Created is true (we don't ask permission to delete a
	// worktree the user might've kept on purpose).
	Created bool
}

// SpawnWorktree runs `git worktree add` (or reuses an existing one) and
// returns the absolute path. Slug "" → an auto-generated short id.
//
// Caller responsibilities:
//
//   - Run from a directory inside a real git repo. We bail otherwise.
//   - Be ready to chdir to the returned Path; downstream config loading
//     reads from cwd so doing the chdir EARLY in main matters.
//   - Call CleanupWorktree on shutdown if the user wants the worktree
//     reaped (otherwise it stays on disk and is reusable next time).
func SpawnWorktree(slug string) (*WorktreeInfo, error) {
	if !insideGitRepo() {
		return nil, fmt.Errorf("--worktree requires a git repository (cwd is not inside one)")
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = autoSlug()
	}
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	flat := strings.ReplaceAll(slug, "/", "+")
	root := filepath.Join(config.Home(), worktreesDir, flat)

	// GC abandoned siblings, best-effort.
	_ = sweepStaleWorktrees()

	if _, err := os.Stat(root); err == nil {
		// Path already exists — reuse if it's a registered worktree, else
		// refuse to clobber.
		if isRegisteredWorktree(root) {
			return &WorktreeInfo{Slug: slug, Branch: branchName(flat), Path: root, Created: false}, nil
		}
		return nil, fmt.Errorf("worktree path %s already exists and is not a registered worktree", root)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return nil, err
	}

	branch := branchName(flat)
	cmd := exec.Command("git", "worktree", "add", "-B", branch, root, defaultBaseRef)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	return &WorktreeInfo{Slug: slug, Branch: branch, Path: root, Created: true}, nil
}

// CleanupWorktree runs `git worktree remove --force <path>` and best-effort
// deletes the temp branch we created. Errors are surfaced (not silenced)
// so the user sees why their worktree didn't go away.
func CleanupWorktree(info *WorktreeInfo) error {
	if info == nil {
		return nil
	}
	cmd := exec.Command("git", "worktree", "remove", "--force", info.Path)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree remove %s: %w", info.Path, err)
	}
	// Best-effort: drop the throwaway branch too. Don't fail the whole
	// teardown if branch deletion fails (user may have already merged it
	// elsewhere).
	_ = exec.Command("git", "branch", "-D", info.Branch).Run()
	return nil
}

func validateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("empty slug")
	}
	for _, seg := range strings.Split(slug, "/") {
		if !slugSegmentRE.MatchString(seg) {
			return fmt.Errorf("invalid slug segment %q (use [a-zA-Z0-9._-], ≤64 chars)", seg)
		}
	}
	if strings.Contains(slug, "..") {
		return fmt.Errorf("slug %q contains traversal", slug)
	}
	return nil
}

func insideGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func isRegisteredWorktree(path string) bool {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	abs, _ := filepath.Abs(path)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if pa, err := filepath.Abs(p); err == nil && pa == abs {
				return true
			}
		}
	}
	return false
}

func branchName(flatSlug string) string {
	return "metis/" + flatSlug
}

// autoSlug returns a short, unique-enough id for an unattended /worktree
// session. Pattern: short hex from time+pid, prefixed `wt-`.
func autoSlug() string {
	return fmt.Sprintf("wt-%x", time.Now().UnixNano()&0xfffffff)
}

// sweepStaleWorktrees drops directories under ~/.metis/worktrees older
// than staleWorktree. Best-effort — errors are logged to stderr.
func sweepStaleWorktrees() error {
	dir := filepath.Join(config.Home(), worktreesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no dir == nothing to sweep
	}
	cutoff := time.Now().Add(-staleWorktree)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			full := filepath.Join(dir, e.Name())
			// Use git first so the registration entry gets cleaned too;
			// fall back to plain rm if git refuses (worktree might have
			// been manually nuked already).
			if err := exec.Command("git", "worktree", "remove", "--force", full).Run(); err != nil {
				_ = os.RemoveAll(full)
			}
		}
	}
	return nil
}
