package runtime

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// inTempGitRepo cds into a fresh git repo with one commit and one dirty
// file, restoring cwd afterwards. Skips when git isn't available.
func inTempGitRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	os.WriteFile("a.txt", []byte("hi"), 0o644)
	run("add", "a.txt")
	run("commit", "-m", "initial commit")
	os.WriteFile("dirty.txt", []byte("uncommitted"), 0o644)
}

func TestBuildGitContext(t *testing.T) {
	inTempGitRepo(t)
	got := buildGitContext()
	if got == "" {
		t.Fatal("buildGitContext returned empty in a git repo")
	}
	for _, want := range []string{
		"<git_status>", "</git_status>",
		"Current branch: main",
		"?? dirty.txt",
		"initial commit",
		"snapshot",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("git context missing %q:\n%s", want, got)
		}
	}
	if len(got) > gitContextMaxBytes+64 { // +64: truncation suffix allowance
		t.Errorf("git context exceeds cap: %d bytes", len(got))
	}
}

func TestBuildGitContextOutsideRepo(t *testing.T) {
	dir := t.TempDir() // TempDir is never inside a repo on darwin/linux CI
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if got := buildGitContext(); got != "" {
		t.Errorf("expected empty outside a repo, got:\n%s", got)
	}
}

// Sub-agent (minimal) prompts must not pay for the git snapshot.
func TestGitContextSkippedForMinimalPrompt(t *testing.T) {
	inTempGitRepo(t)
	secs := AssembleSystemPromptSections("base prompt", AssembleOptions{Mode: PromptMinimal})
	for _, s := range secs {
		if s.Name == "git_context" {
			t.Error("git_context section present in PromptMinimal assembly")
		}
	}
	secs = AssembleSystemPromptSections("base prompt", AssembleOptions{Mode: PromptFull})
	found := false
	for _, s := range secs {
		if s.Name == "git_context" {
			found = true
			if s.Cache || !s.Volatile {
				t.Error("git_context must be Cache=false Volatile=true")
			}
		}
	}
	if !found {
		t.Error("git_context section missing from PromptFull assembly inside a repo")
	}
}
