//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestMemoryDistillationWorkerGetsIndependentProcessSession(t *testing.T) {
	cmd := exec.Command("unused-fixture")
	configureMemoryDistillationProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("worker can inherit foreground terminal signals")
	}
}

// This is a test subprocess, not the CLI dispatcher or a model request. A
// fresh process is necessary to expose auth's process-local home cache.
func TestMemoryDistillationConfigHomeChild(t *testing.T) {
	if os.Getenv("METIS_DISTILL_HOME_CHILD") != "1" {
		return
	}
	home, err := freezeMemoryDistillationConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	if home != os.Getenv("METIS_DISTILL_EXPECTED_HOME") {
		t.Fatal("child selected a different configuration namespace")
	}
	if err := os.Chdir(os.Getenv("METIS_DISTILL_NEXT_WORKSPACE")); err != nil {
		t.Fatal(err)
	}
	after, err := config.VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if after != home || os.Getenv("METIS_HOME") != home {
		t.Fatal("workspace switch changed the pinned credential namespace")
	}
}

func runMemoryDistillationHomeChild(t *testing.T, env []string, home, cwd, nextWorkspace string) error {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	if env == nil {
		// Preserve the production launcher's home capture and environment;
		// replace only the executable arguments with this bounded test helper.
		cmd, err = memoryDistillationWorkerCommand(executable, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
	} else {
		cmd = exec.Command(executable)
		cmd.Env = env
	}
	cmd.Args = []string{executable, "-test.run=^TestMemoryDistillationConfigHomeChild$"}
	cmd.Dir = cwd
	cmd.Env = append(cmd.Env, "METIS_DISTILL_HOME_CHILD=1", "METIS_DISTILL_EXPECTED_HOME="+home, "METIS_DISTILL_NEXT_WORKSPACE="+nextWorkspace)
	_, err = cmd.CombinedOutput()
	return err
}

func TestMemoryDistillationWorkerPinsRelativeConfigurationHomeAcrossWorkspaces(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	t.Chdir(source)
	t.Setenv("METIS_HOME", ".relative-profile")
	home, err := config.VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := runMemoryDistillationHomeChild(t, os.Environ(), home, target, source); err == nil {
		t.Fatal("negative control did not expose relative home reinterpretation")
	}
	if err := runMemoryDistillationHomeChild(t, nil, home, target, source); err != nil {
		t.Fatalf("canonical home was not retained across child workspaces: %v", err)
	}
}

func TestMemoryDistillationWorkerPinsSymlinkHomeBeforeChildLaunch(t *testing.T) {
	parent, source, target := t.TempDir(), t.TempDir(), t.TempDir()
	homeA, homeB := filepath.Join(parent, "profile-a"), filepath.Join(parent, "profile-b")
	for _, home := range []string{homeA, homeB} {
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(parent, "profile")
	if err := os.Symlink(homeA, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	t.Chdir(source)
	t.Setenv("METIS_HOME", link)
	frozenHome, err := config.VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(homeB, link); err != nil {
		t.Fatal(err)
	}
	// The foreground's verifier retains profile A, while a fresh process
	// inheriting the lexical symlink would otherwise silently select B.
	verified, err := config.VerifiedHome()
	if err != nil || verified != frozenHome {
		t.Fatalf("foreground lost frozen home: %v", err)
	}
	if err := runMemoryDistillationHomeChild(t, os.Environ(), frozenHome, target, source); err == nil {
		t.Fatal("negative control did not expose retargeted home in a fresh child")
	}
	if err := runMemoryDistillationHomeChild(t, nil, frozenHome, target, source); err != nil {
		t.Fatalf("worker followed the retargeted home symlink: %v", err)
	}
}
