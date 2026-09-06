package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	worktreepkg "github.com/Ricardo-M-L/metis/internal/worktree"
)

func worktreeTrustFixture(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	t.Setenv("METIS_NO_TRUST_PROMPT", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("METIS_WORKTREE_TEST_KEY", "fake-worktree-key")
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(".metis", 0o700); err != nil {
		t.Fatal(err)
	}
	body := `[provider]
default = "project-route"
[provider.custom.project-route]
transport = "openai_chat"
base_url = "http://127.0.0.1:1/v1"
model = "project-model"
api_key_env = "METIS_WORKTREE_TEST_KEY"
`
	if err := os.WriteFile(filepath.Join(".metis", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", ".metis/config.toml"}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := addTrustedDir(cwd); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestChatWorktreeTrustConfirmsTargetBeforeProviderPolicy(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "new"
		if reuse {
			name = "reused"
		}
		t.Run(name, func(t *testing.T) {
			project := worktreeTrustFixture(t)
			if reuse {
				if _, err := worktreepkg.Spawn("review-target"); err != nil {
					t.Fatal(err)
				}
			}
			var confirmed string
			info, err := prepareChatWorkspace(&cliFlags{worktree: "review-target"}, func() error {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				confirmed = cwd
				return addTrustedDir(cwd)
			})
			if err != nil {
				t.Fatal(err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if info.Created == reuse || confirmed != cwd || confirmed == project {
				t.Errorf("trust confirmed %q; actual workspace %q, created=%v", confirmed, cwd, info.Created)
			}
			cfg, _, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := config.ApplyProviderPolicyForWorkspace(cfg, currentWorkspaceTrusted()); err != nil {
				t.Fatal(err)
			}
			if cfg.Provider.Default != "project-route" {
				t.Fatalf("spawned workspace lost project provider: got %q, trusted=%v", cfg.Provider.Default, currentWorkspaceTrusted())
			}
			if _, err := rtpkg.BuildProviderWithoutPreconnect(cfg, cfg.Provider.Default, ""); err != nil {
				t.Fatalf("build project provider: %v", err)
			}
		})
	}
}

func TestChatWorktreeDoesNotInheritParentTrust(t *testing.T) {
	for _, prompt := range []string{"noninteractive", "declined", "disabled"} {
		t.Run(prompt, func(t *testing.T) {
			worktreeTrustFixture(t)
			var confirm func() error
			declined := errors.New("directory not trusted")
			if prompt == "declined" {
				confirm = func() error { return declined }
			} else if prompt == "disabled" {
				t.Setenv("METIS_NO_TRUST_PROMPT", "1")
				confirm = ensureTrusted
			}
			_, err := prepareChatWorkspace(&cliFlags{worktree: "untrusted-target"}, confirm)
			if prompt == "declined" && !errors.Is(err, declined) {
				t.Fatalf("declining target trust = %v", err)
			} else if prompt != "declined" && err != nil {
				t.Fatal(err)
			}
			if currentWorkspaceTrusted() {
				t.Fatal("target inherited parent trust without affirmative confirmation")
			}
			cfg, _, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := config.ApplyProviderPolicyForWorkspace(cfg, false); err != nil {
				t.Fatal(err)
			}
			if _, exists := cfg.Provider.Custom["project-route"]; exists {
				t.Fatal("untrusted target retained project provider routing")
			}
		})
	}
}
