package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// resetScopeCache clears the per-dir worktree-detection cache so a
// test doesn't see another test's stale answer.
func resetScopeCache(t *testing.T) {
	t.Helper()
	scopeCacheMu.Lock()
	scopeCache = map[string]scopeEntry{}
	scopeCacheMu.Unlock()
}

func TestInsideGitWorktree_TempDirNotInRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	if insideGitWorktree(dir) {
		t.Errorf("fresh tempdir %q should not be inside a worktree", dir)
	}
}

func TestInsideGitWorktree_AfterGitInit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "--quiet")
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if !insideGitWorktree(filepath.Clean(abs)) {
		t.Errorf("post-`git init` dir %q should be inside a worktree", abs)
	}
}

func TestInsideGitWorktree_CacheRespectsTTL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	// Prime the cache as "not inside".
	if insideGitWorktree(dir) {
		t.Skip("dir already in worktree, can't run this test")
	}
	// Now make it a worktree.
	if err := exec.Command("git", "-C", dir, "init", "--quiet").Run(); err != nil {
		t.Skip("git init failed")
	}
	// Cache still says not-inside (TTL not expired) — verifies caching
	// is actually doing its job.
	if insideGitWorktree(dir) {
		t.Errorf("cache should still report not-inside within TTL")
	}
	// Fast-forward by clobbering the cache entry's timestamp.
	scopeCacheMu.Lock()
	for k, v := range scopeCache {
		v.at = time.Now().Add(-2 * scopeCacheTTL)
		scopeCache[k] = v
	}
	scopeCacheMu.Unlock()
	if !insideGitWorktree(dir) {
		t.Errorf("after TTL expiry, fresh check should report inside")
	}
}

func TestWalkBudget_NotInWorktreeClampsToSmallDefaults(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	depth, items := walkBudget(dir)
	if depth == 0 || items == 0 {
		t.Errorf("non-worktree should yield clamps, got depth=%d items=%d", depth, items)
	}
	if depth > 8 {
		t.Errorf("non-worktree depth cap too loose: %d", depth)
	}
	if items > 500 {
		t.Errorf("non-worktree item cap too loose: %d", items)
	}
}

func TestWalkBudget_InWorktreeReturnsZeroes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "--quiet").Run(); err != nil {
		t.Skip("git init failed")
	}
	depth, items := walkBudget(dir)
	if depth != 0 || items != 0 {
		t.Errorf("inside worktree should yield (0,0) so caller's defaults stand, got (%d,%d)", depth, items)
	}
}

func TestInsideGitWorktree_EmptyDirReturnsFalse(t *testing.T) {
	if insideGitWorktree("") {
		t.Errorf("empty dir should not be reported as inside a worktree")
	}
}

// Sanity: verify $HOME is (almost certainly) not a git work tree on
// dev machines — protects the "running from $HOME" path. If a dev's
// $HOME *is* a worktree (rare but legal), this will skip rather than
// fail.
func TestInsideGitWorktree_HomeNotInWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if insideGitWorktree(home) {
		t.Skipf("$HOME=%q is a worktree on this machine, can't assert non-membership", home)
	}
}
