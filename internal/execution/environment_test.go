package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStoreRecordsAndMergesWorkspaceFailure(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(filepath.Join(t.TempDir(), "profiles"))
	profile, err := Probe(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if profile.IsGitRepository {
		t.Fatalf("temporary fixture unexpectedly looks like a Git repository: %+v", profile)
	}
	failure := Failure{
		Code:            FailureWorktreeRequiresGit,
		Summary:         "Git worktree isolation is unavailable because this workspace is not a Git repository.",
		SuggestedAction: "Use direct execution with the same working directory.",
		AutoRecovered:   true,
	}
	if err := store.Record(profile, failure); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(profile, failure); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.LastFailure == nil {
		t.Fatalf("stored profile = %+v, found=%v", got, found)
	}
	if got.LastFailure.Code != FailureWorktreeRequiresGit || got.LastFailure.Occurrences != 2 || !got.LastFailure.AutoRecovered {
		t.Fatalf("stored failure = %+v", got.LastFailure)
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("profile files = %v, %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %o, want 600", info.Mode().Perm())
	}
}

func TestProbeRecognizesRepositoryAndLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	profile, err := Probe(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.IsGitRepository || profile.GitRoot == "" || profile.IsLinkedWorktree {
		t.Fatalf("main checkout profile = %+v", profile)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := append([]string{"-C", dir}, args...)
	if out, err := gitCommand(command...); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitCommand(args ...string) ([]byte, error) {
	return exec.Command("git", args...).CombinedOutput()
}
