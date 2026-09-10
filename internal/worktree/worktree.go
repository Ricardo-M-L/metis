// Package worktree spawns + manages `git worktree` checkouts.
//
// Moved out of `internal/runtime` 2026-05-12 (Phase G.2) so that the
// builtin Agent tool can call `Spawn` directly without creating an
// import cycle (builtin → runtime would loop because runtime already
// imports builtin to wire the registry).
//
// Main entry points:
//
//   - `Spawn(slug)` — the original `metis -W <slug>` entry using the
//     process working directory.
//
//   - `SpawnFrom(dir, slug)` — the per-Agent entry that can select a
//     source repository independently from the process working directory.
//
// Both reuse an existing worktree if the slug matches and refuse to clobber
// unrelated directories.
//
//   - `Cleanup(info)` — `git worktree remove --force` + branch GC.
//
// Mirrors claude-code's worktree mode: when the flag is set, metis
// runs `git worktree add` to spawn an isolated checkout under
// ~/.metis/worktrees/<slug> and chdirs into it before normal startup.
// On exit (Ctrl+D / `:q` / process kill) the user gets a prompt to
// either keep the worktree on disk or `git worktree remove --force`.
//
// Safety:
//   - Slug is validated against [a-zA-Z0-9._-]{1,64} segments separated
//     by `/` (which we flatten to `+` for the actual branch / dir name).
//   - Refuse to clobber an existing worktree directory unless the slug
//     resolves to the same head SHA (cheap fast-resume path).
//   - Stale worktrees older than 30 days under ~/.metis/worktrees/
//     are GC'd on every spawn so abandoned slugs don't pile up.
package worktree

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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

var ErrNotGitRepository = errors.New("directory is not inside a git repository")

// Info is what Spawn and SpawnFrom return to the caller. The caller chdirs into
// Path before continuing setup.
type Info struct {
	Slug     string
	Branch   string
	Path     string
	RepoRoot string
	// Created is true when this invocation actually ran `git worktree add`,
	// false when it reused an existing one. The teardown prompt only
	// applies when Created is true (we don't ask permission to delete a
	// worktree the user might've kept on purpose).
	Created bool
}

// Spawn runs `git worktree add` (or reuses an existing one) and
// returns the absolute path. Slug "" → an auto-generated short id
// via AutoSlug().
//
// Caller responsibilities:
//
//   - Run from a directory inside a real git repo. We bail otherwise.
//   - Be ready to chdir to the returned Path (or pass it as cwd for a
//     sub-agent's `exec.Cmd`); downstream config loading reads from cwd
//     so doing the chdir EARLY in main matters.
//   - Call Cleanup on shutdown if the worktree should be reaped
//     (otherwise it stays on disk and is reusable next time).
func Spawn(slug string) (*Info, error) {
	return SpawnFrom("", slug)
}

// SpawnFrom creates a linked worktree from the repository containing sourceDir.
// An empty sourceDir preserves Spawn's process-cwd behavior.
func SpawnFrom(sourceDir, slug string) (*Info, error) {
	if sourceDir == "" {
		var err error
		sourceDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get cwd for worktree spawn: %w", err)
		}
	}
	repoRoot, err := repositoryRoot(sourceDir)
	if err != nil {
		return nil, err
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = AutoSlug()
	}
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	flat := strings.ReplaceAll(slug, "/", "+")
	root := filepath.Join(config.Home(), worktreesDir, flat)

	// GC abandoned siblings, best-effort.
	_ = sweepStaleWorktrees(repoRoot)

	if _, err := os.Stat(root); err == nil {
		// Path already exists — reuse if it's a registered worktree, else
		// refuse to clobber.
		if isRegisteredWorktree(repoRoot, root) {
			return &Info{Slug: slug, Branch: branchName(flat), Path: root, RepoRoot: repoRoot, Created: false}, nil
		}
		return nil, fmt.Errorf("worktree path %s already exists and is not a registered worktree", root)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return nil, err
	}

	branch := branchName(flat)
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-B", branch, root, defaultBaseRef)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	return &Info{Slug: slug, Branch: branch, Path: root, RepoRoot: repoRoot, Created: true}, nil
}

// Cleanup runs `git worktree remove --force <path>` and best-effort
// deletes the temp branch. Errors are surfaced (not silenced) so the
// user sees why their worktree didn't go away.
func Cleanup(info *Info) error {
	if info == nil {
		return nil
	}
	repoRoot := info.RepoRoot
	if repoRoot == "" {
		var err error
		repoRoot, err = repositoryRoot(info.Path)
		if err != nil {
			return fmt.Errorf("resolve repository for worktree cleanup: %w", err)
		}
	}
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", info.Path)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree remove %s: %w", info.Path, err)
	}
	// Best-effort: drop the throwaway branch too. Don't fail the whole
	// teardown if branch deletion fails (user may have already merged it
	// elsewhere).
	_ = exec.Command("git", "-C", repoRoot, "branch", "-D", info.Branch).Run()
	return nil
}

// InsideWorktree reports whether path belongs to a linked worktree rather
// than the repository's main checkout. Used by G.2's nested-worktree guard so an
// Agent({isolation:"worktree"}) call inside `metis -W feat1` produces
// a clear "refusing nested worktree" tool error instead of cascading
// branches.
func InsideWorktree(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	gitDir, err := gitOutput(abs, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	if err != nil {
		return false
	}
	commonDir, err := gitOutput(abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false
	}
	return canonicalPath(gitDir) != canonicalPath(commonDir)
}

// AutoSlug returns a short, unique id for unattended worktree
// invocations. Format `wt-<8hex>` — short enough to read in
// `git worktree list` output and unique enough that two parallel
// Agent calls don't collide (8 hex bytes = 4 billion namespace).
//
// 2026-05-12: was `wt-<unix-nano-mask>` which collided when two
// sub-agents spawned within the same nanosecond from G.1's parallel
// dispatch. Switched to crypto/rand-backed hex for actual uniqueness.
func AutoSlug() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback only when the system RNG fails — vanishingly rare,
		// but better to keep the legacy time-mixed slug than panic.
		return fmt.Sprintf("wt-%x", time.Now().UnixNano()&0xfffffff)
	}
	return "wt-" + hex.EncodeToString(b[:])
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

func repositoryRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve worktree source %q: %w", path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("worktree source %q: %w", abs, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("worktree source %q is not a directory", abs)
	}
	root, err := gitOutput(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotGitRepository, abs)
	}
	return canonicalPath(root), nil
}

func gitOutput(dir string, args ...string) (string, error) {
	gitArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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

func isRegisteredWorktree(repoRoot, path string) bool {
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	abs := canonicalPath(path)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if canonicalPath(p) == abs {
				return true
			}
		}
	}
	return false
}

func branchName(flatSlug string) string {
	return "metis/" + flatSlug
}

// sweepStaleWorktrees drops directories under ~/.metis/worktrees older
// than staleWorktree. Best-effort — errors are logged to stderr.
func sweepStaleWorktrees(repoRoot string) error {
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
			// The global worktrees directory can contain checkouts from many
			// repositories. Only sweep entries registered to this repository.
			if isRegisteredWorktree(repoRoot, full) {
				_ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", full).Run()
			}
		}
	}
	return nil
}
