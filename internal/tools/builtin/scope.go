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
// (NOT walking up looking for `.git`) — git knows about linked
// worktrees, submodules, and repository discovery rules;
// reimplementing those rules would diverge.

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
)

const (
	scopeGitTimeout              = 800 * time.Millisecond
	scopeProcessKillDispatchWait = 250 * time.Millisecond
	scopeProcessWaitLimit        = 2 * time.Second
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
	return insideGitWorktreeWithSandbox(dir, nil)
}

// insideGitWorktreeWithSandbox is the backwards-compatible entry point for
// callers that do not have an invocation context. Runtime tools use
// insideGitWorktreeContext so turn cancellation also reaches the git probe.
func insideGitWorktreeWithSandbox(dir string, manager *sandbox.Manager) bool {
	return insideGitWorktreeContext(context.Background(), dir, manager)
}

func insideGitWorktreeContext(ctx context.Context, dir string, manager *sandbox.Manager) bool {
	if dir == "" {
		return false
	}
	scopeCacheMu.Lock()
	if e, ok := scopeCache[dir]; ok && time.Since(e.at) < scopeCacheTTL {
		scopeCacheMu.Unlock()
		return e.inside
	}
	scopeCacheMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, scopeGitTimeout)
	defer cancel()
	cmd, err := newScopeGitCommand(runCtx, dir, manager)
	if err != nil {
		cacheScopeResult(dir, false)
		return false
	}
	cmd.Stderr = nil
	cmd.Stdout = nil
	inside := cmd.Run() == nil

	cacheScopeResult(dir, inside)
	return inside
}

func cacheScopeResult(dir string, inside bool) {
	scopeCacheMu.Lock()
	scopeCache[dir] = scopeEntry{inside: inside, at: time.Now()}
	scopeCacheMu.Unlock()
}

// newScopeGitCommand applies the same child-process boundary used by the
// model-controlled command tools. The worktree probe is fixed, but its -C
// directory is supplied by Glob/Grep/LS and git still reads config while
// starting, so it must neither inherit provider credentials nor escape the
// runtime's filesystem policy.
func newScopeGitCommand(ctx context.Context, dir string, manager *sandbox.Manager) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	env := security.RestrictedSubprocessEnv(os.Environ())
	if manager != nil {
		// FilterEnv keeps the already allow-listed environment restricted while
		// routing temporary files into this runtime's private sandbox directory.
		env = manager.FilterEnv(env, false)
	}
	cmd.Env = env
	configureScopeProcess(cmd)
	if manager == nil {
		return cmd, nil
	}
	return manager.Wrap(cmd, sandbox.Request{Cwd: dir, Network: sandbox.NetworkBlock})
}

// configureScopeProcess isolates git and any helper it may start, then makes
// context cancellation kill the complete process tree. WaitDelay bounds
// os/exec cleanup even if a descendant escapes the group or inherits a pipe
// handle on a platform without Unix process groups.
func configureScopeProcess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	jobs.ApplyProcessGroup(cmd)
	cmd.Cancel = func() error {
		killScopeProcessTree(cmd)
		return nil
	}
	cmd.WaitDelay = scopeProcessWaitLimit
}

// killScopeProcessTree bounds the platform tree-kill primitive itself. On
// Windows that primitive shells out to taskkill; a stuck taskkill must not
// wedge the goroutine os/exec uses to process context cancellation. Killing
// the leader directly remains the final fallback on every platform.
func killScopeProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	process := cmd.Process
	treeKillDone := make(chan struct{})
	go func() {
		jobs.KillProcessGroup(process)
		close(treeKillDone)
	}()

	timer := time.NewTimer(scopeProcessKillDispatchWait)
	select {
	case <-treeKillDone:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
	}
	_ = process.Kill()
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
	return walkBudgetWithSandbox(context.Background(), dir, nil)
}

func walkBudgetWithSandbox(ctx context.Context, dir string, manager *sandbox.Manager) (maxDepth, maxItems int) {
	if insideGitWorktreeContext(ctx, dir, manager) {
		return 0, 0
	}
	return 4, 200
}
