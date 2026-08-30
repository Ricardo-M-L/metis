package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
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

func TestNewScopeGitCommandUsesRestrictedEnvironmentAndBoundedCancellation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("OPENAI_API_KEY", "scope-must-not-inherit-this")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := newScopeGitCommand(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Env, "\n")
	if strings.Contains(joined, "OPENAI_API_KEY=") || strings.Contains(joined, "scope-must-not-inherit-this") {
		t.Fatalf("scope git inherited provider credentials: %s", joined)
	}
	for _, want := range []string{"AGENT=metis", "AI_AGENT=metis", "METIS=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("scope git env missing %q: %v", want, cmd.Env)
		}
	}
	if cmd.Cancel == nil {
		t.Fatal("scope git command has no process-tree cancellation hook")
	}
	if cmd.WaitDelay != scopeProcessWaitLimit {
		t.Fatalf("scope git WaitDelay = %s, want %s", cmd.WaitDelay, scopeProcessWaitLimit)
	}
}

func TestInsideGitWorktreeSandboxWrapFailureFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "--quiet").Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if insideGitWorktreeWithSandbox(dir, manager) {
		t.Fatal("closed sandbox manager must fail closed instead of running git unsandboxed")
	}
}

func TestScopeToolsRetainSandboxManager(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeBypassPermissions)

	checks := []struct {
		name string
		got  *sandbox.Manager
	}{
		{name: "NewLSWithSandbox", got: NewLSWithSandbox(gate, manager).SandboxManager()},
		{name: "LS.WithSandbox", got: NewLS(gate).WithSandbox(manager).SandboxManager()},
		{name: "NewGlobWithSandbox", got: NewGlobWithSandbox(gate, manager).SandboxManager()},
		{name: "Glob.WithSandbox", got: NewGlob(gate).WithSandbox(manager).SandboxManager()},
		{name: "NewGrepWithSandbox", got: NewGrepWithSandbox(gate, manager).SandboxManager()},
		{name: "Grep.WithSandbox", got: NewGrep(gate).WithSandbox(manager).SandboxManager()},
	}
	for _, check := range checks {
		if check.got != manager {
			t.Errorf("%s manager = %p, want %p", check.name, check.got, manager)
		}
	}
}

func TestRegisterWithSandboxInjectsManagerIntoScopeTools(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	cfg := &config.Config{}
	cfg.Session.SkillDir = filepath.Join(t.TempDir(), "skills")
	cfg.Session.Dir = filepath.Join(t.TempDir(), "session")
	registry := tools.NewRegistry()
	RegisterWithSandbox(registry, cfg, permission.New(permission.ModeBypassPermissions), manager)

	checks := []struct {
		name string
		get  func(tools.Tool) *sandbox.Manager
	}{
		{name: "LS", get: func(tool tools.Tool) *sandbox.Manager { return tool.(LS).SandboxManager() }},
		{name: "Glob", get: func(tool tools.Tool) *sandbox.Manager { return tool.(Glob).SandboxManager() }},
		{name: "Grep", get: func(tool tools.Tool) *sandbox.Manager { return tool.(Grep).SandboxManager() }},
	}
	for _, check := range checks {
		registered, ok := registry.Get(check.name)
		if !ok {
			t.Fatalf("%s was not registered", check.name)
		}
		if got := check.get(registered); got != manager {
			t.Errorf("registered %s manager = %p, want %p", check.name, got, manager)
		}
	}
}

// TestLS_TruncatesOutsideWorktree is an end-to-end check: drop 300
// files into a non-git tempdir and confirm LS.Execute trims the
// listing + appends the [truncated …] footer the agent sees.
// Mirrors the real out-of-worktree fan-out scenario from the
// 2026-05-05 incident.
func TestLS_TruncatesOutsideWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	for i := 0; i < 300; i++ {
		f := filepath.Join(dir, fmt.Sprintf("f_%03d.txt", i))
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := LS{gate: bypassGate()}
	res, err := l.Execute(context.Background(), map[string]any{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("expected truncation footer in non-worktree output; got first 500 bytes: %q", res.Output[:min(len(res.Output), 500)])
	}
	// Count how many file rows actually rendered — should be ≤ 200
	// (walkBudget's outside-worktree cap), not the full 300.
	rows := strings.Count(res.Output, "f_")
	if rows > 200 {
		t.Errorf("expected ≤ 200 rows under non-worktree clamp, got %d", rows)
	}
	if rows == 0 {
		t.Errorf("expected some rows, got zero — clamp too aggressive?")
	}
}

// TestLS_NoTruncationInsideWorktree confirms the clamp is gated on
// the no-git-repo signal: same 300 files in a `git init`-ed dir
// should render in full.
func TestLS_NoTruncationInsideWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	resetScopeCache(t)
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "--quiet").Run(); err != nil {
		t.Skip("git init failed")
	}
	for i := 0; i < 300; i++ {
		f := filepath.Join(dir, fmt.Sprintf("f_%03d.txt", i))
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := LS{gate: bypassGate()}
	res, err := l.Execute(context.Background(), map[string]any{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "truncated") {
		t.Errorf("expected NO truncation inside worktree; got truncated footer")
	}
	rows := strings.Count(res.Output, "f_")
	if rows < 300 {
		t.Errorf("expected all 300 rows inside worktree, got %d", rows)
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
