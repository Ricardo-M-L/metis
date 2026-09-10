package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	good := []string{"feature-x", "fix.123", "release_2026", "stack/page", "a"}
	for _, s := range good {
		if err := validateSlug(s); err != nil {
			t.Errorf("validateSlug(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "..", "a/..", "/leading", "trailing/", "with space", strings.Repeat("a", 65)}
	for _, s := range bad {
		if err := validateSlug(s); err == nil {
			t.Errorf("validateSlug(%q) should error", s)
		}
	}
}

func TestBranchName(t *testing.T) {
	if got := branchName("feature-x"); got != "metis/feature-x" {
		t.Errorf("branchName: %q", got)
	}
}

// TestAutoSlugUnique — the 2026-05-12 collision fix. The previous
// `time.Now().UnixNano() & 0xfffffff` masked slug collided when two
// sub-agents spawned within the same nanosecond from G.1's parallel
// dispatch. Switched to crypto/rand-backed hex so 1000 rapid calls
// produce 1000 distinct slugs.
func TestAutoSlugUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		s := AutoSlug()
		if !strings.HasPrefix(s, "wt-") {
			t.Fatalf("iter %d: AutoSlug missing prefix: %q", i, s)
		}
		if err := validateSlug(s); err != nil {
			t.Fatalf("iter %d: AutoSlug %q failed validation: %v", i, s, err)
		}
		if seen[s] {
			t.Fatalf("iter %d: duplicate slug %q (collision broke parallel-spawn safety)", i, s)
		}
		seen[s] = true
	}
}

func TestInsideWorktreeDistinguishesMainCheckoutFromLinkedWorktree(t *testing.T) {
	repo := newWorktreeTestRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runWorktreeTestGit(t, repo, "worktree", "add", "--detach", linked, "HEAD")
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", linked).Run()
	})

	if InsideWorktree(repo) {
		t.Fatal("main checkout must not be classified as a nested linked worktree")
	}
	if !InsideWorktree(linked) {
		t.Fatal("linked checkout must be classified as a worktree")
	}
}

func TestSpawnFromUsesExplicitRepository(t *testing.T) {
	repo := newWorktreeTestRepo(t)
	outside := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(outside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	t.Setenv("METIS_HOME", filepath.Join(t.TempDir(), "metis-home"))

	info, err := SpawnFrom(repo, "explicit-repo")
	if err != nil {
		t.Fatalf("SpawnFrom: %v", err)
	}
	t.Cleanup(func() { _ = Cleanup(info) })
	if info.RepoRoot != canonicalPath(repo) {
		t.Fatalf("RepoRoot = %q, want %q", info.RepoRoot, canonicalPath(repo))
	}
	if !InsideWorktree(info.Path) {
		t.Fatalf("spawned path %q is not recognized as a linked worktree", info.Path)
	}
	if err := Cleanup(info); err != nil {
		t.Fatalf("Cleanup from unrelated process cwd: %v", err)
	}
}

func newWorktreeTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runWorktreeTestGit(t, repo, "init", "--quiet")
	runWorktreeTestGit(t, repo, "config", "user.name", "Metis Worktree Test")
	runWorktreeTestGit(t, repo, "config", "user.email", "metis-worktree@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorktreeTestGit(t, repo, "add", "README.md")
	runWorktreeTestGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
	return repo
}

func runWorktreeTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
