package builtin

// scope.go — "is the agent rooted in a git work tree?" gate that
// shrinks the default walk budget for Glob / Grep / LS when it
// isn't.
//
// Why: when metis runs from $HOME, /tmp, /var, or any random dir
// outside a repo, Glob("**/*.go") and Grep("foo") will happily
// enumerate every file the user has ever cached — minutes of latency
// returning thousands of paths the model never reads. The 2026-05-05
// 41-second incident (image #5) was Glob walking $HOME/Library; this
// gate generalises that fix to ANY non-repo cwd by detecting "no
// .git ancestor" and clamping depth + result count.
//
// Mirrors Crush's `tools/internal/scope.go`'s `IsInsideWorktree`
// guard — Crush also drops MaxDepth=2 / MaxItems=100 outside a repo.
//
// Detection is via `git -C <dir> rev-parse --is-inside-work-tree`
// (NOT walking up looking for `.git`) — git knows about worktrees,
// submodules, and `GIT_DIR` env overrides; reimplementing the rules
// would diverge.

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// scopeCache memoises the worktree check per cleaned absolute root
// for a short window. The git subprocess is ~5ms cold; without the
// cache, every Glob/Grep/LS call would re-fork. The TTL is short so
// `cd` into a repo (or `git init`) reflects within a turn.
var (
	scopeCacheMu  sync.Mutex
	scopeCache    = map[string]scopeEntry{}
	scopeCacheTTL = 5 * time.Second
)

type scopeEntry struct {
	inside bool
	at     time.Time
}

// insideGitWorktree reports whether `dir` (an absolute, cleaned
// path) sits inside a git work tree. Uses a 5-second per-dir cache
// to avoid forking git on every tool call. Returns false on any
// error (no git binary, dir doesn't exist, etc.) — the caller's
// "not in a worktree → tighter limits" path is the safe default.
func insideGitWorktree(dir string) bool {
	if dir == "" {
		return false
	}
	scopeCacheMu.Lock()
	if e, ok := scopeCache[dir]; ok && time.Since(e.at) < scopeCacheTTL {
		scopeCacheMu.Unlock()
		return e.inside
	}
	scopeCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	cmd.Stderr = nil
	cmd.Stdout = nil
	inside := cmd.Run() == nil

	scopeCacheMu.Lock()
	scopeCache[dir] = scopeEntry{inside: inside, at: time.Now()}
	scopeCacheMu.Unlock()
	return inside
}

// walkBudget returns (maxDepth, maxItems) defaults for a directory
// walk rooted at `dir`. When `dir` is inside a git work tree, the
// caller's existing defaults stand (returns 0, 0 — caller keeps its
// own values). When `dir` is OUTSIDE any work tree, the budget is
// clamped: depth 4, items 200. These are loose enough for "I'm in
// /tmp/scratch, look at the files I just created" but tight enough
// that "Glob('**/*') from $HOME" can't scan 50 GB of cache.
//
// 4 / 200 picked from Crush's defaults; metis's $HOME-specific cap
// (depth 8) stays in effect when applicable — walkBudget is the
// catch-all for non-$HOME, non-repo dirs.
func walkBudget(dir string) (maxDepth, maxItems int) {
	if insideGitWorktree(dir) {
		return 0, 0
	}
	return 4, 200
}
