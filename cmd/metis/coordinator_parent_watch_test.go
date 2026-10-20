package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorParentWatchCancelsOnPIDReuseOrDeath(t *testing.T) {
	for _, dead := range []bool{false, true} {
		t.Run(map[bool]string{false: "reused", true: "dead"}[dead], func(t *testing.T) {
			var changed atomic.Bool
			probe := func(int) (string, error) {
				if changed.Load() {
					if dead {
						return "", errors.New("gone")
					}
					return "new-birth", nil
				}
				return "old-birth", nil
			}
			ctx, closeWatch, err := coordinatorWatchParent(context.Background(), 100, "old-birth", time.Millisecond, probe)
			if err != nil {
				t.Fatal(err)
			}
			defer closeWatch()
			changed.Store(true)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("worker continued after parent identity changed")
			}
		})
	}
}

func TestCoordinatorParentProcessHelper(t *testing.T) {
	mode := os.Getenv("METIS_PARENT_TEST_HELPER")
	if mode == "" {
		return
	}
	directory := os.Getenv("METIS_PARENT_TEST_DIRECTORY")
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestCoordinatorParentProcessHelper$")
		child.Env = append(os.Environ(), "METIS_PARENT_TEST_HELPER=child")
		child.Env = append(child.Env, coordinatorParentEnvironment()...)
		if err := child.Start(); err != nil {
			os.Exit(31)
		}
		if err := os.WriteFile(filepath.Join(directory, "child-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(32)
		}
		_ = child.Wait()
		os.Exit(0)
	}
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, closeWatch, err := coordinatorChildContext(parent)
	if err != nil {
		os.Exit(33)
	}
	defer closeWatch()
	if err := os.WriteFile(filepath.Join(directory, "watch-ready"), nil, 0600); err != nil {
		os.Exit(34)
	}
	<-ctx.Done()
	if parent.Err() != nil {
		os.Exit(35)
	} // A timeout is never evidence of parent-death recovery.
	if err := os.WriteFile(filepath.Join(directory, "parent-death-observed"), nil, 0600); err != nil {
		os.Exit(36)
	}
	os.Exit(0)
}

func TestCoordinatorParentDeathCancelsRealChild(t *testing.T) {
	if goruntime.GOOS != "darwin" && goruntime.GOOS != "linux" {
		t.Skip("native parent lifetime guard is supported on macOS and Linux")
	}
	directory := t.TempDir()
	parent := exec.Command(os.Args[0], "-test.run=^TestCoordinatorParentProcessHelper$")
	parent.Env = append(os.Environ(), "METIS_PARENT_TEST_HELPER=parent", "METIS_PARENT_TEST_DIRECTORY="+directory)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, "watch-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child watcher did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	childPIDBytes, err := os.ReadFile(filepath.Join(directory, "child-pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	childBirth, err := coordinatorProcessBirthIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if current, err := coordinatorProcessBirthIdentity(childPID); err == nil && current == childBirth {
			if child, err := os.FindProcess(childPID); err == nil {
				_ = child.Kill()
			}
		}
	}()
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, "parent-death-observed")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual parent SIGKILL did not cancel its child")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCoordinatorParentIdentityCurrentProcess(t *testing.T) {
	if goruntime.GOOS != "darwin" && goruntime.GOOS != "linux" {
		t.Skip("native process birth identity is supported on macOS and Linux")
	}
	a, err := coordinatorProcessBirthIdentity(os.Getpid())
	if err != nil || a == "" {
		t.Fatalf("birth identity: %q %v", a, err)
	}
	b, err := coordinatorProcessBirthIdentity(os.Getpid())
	if err != nil || b != a {
		t.Fatalf("process identity changed: %q %q %v", a, b, err)
	}
	if got := strings.Join(coordinatorParentEnvironment(), "\n"); !strings.Contains(got, coordinatorParentIdentityEnv+"="+a) {
		t.Fatalf("wrong parent env: %s", got)
	}
}

func TestCoordinatorChildContextRejectsIncompleteParent(t *testing.T) {
	t.Setenv(coordinatorParentPIDEnv, "bad")
	t.Setenv(coordinatorParentIdentityEnv, "birth")
	if _, _, err := coordinatorChildContext(context.Background()); err == nil {
		t.Fatal("accepted bad parent")
	}
	t.Setenv(coordinatorParentPIDEnv, "")
	t.Setenv(coordinatorParentIdentityEnv, "")
	ctx, closeWatch, err := coordinatorChildContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeWatch()
	if ctx.Err() == nil {
		t.Fatal("unmanaged child cleanup failed")
	}
}
