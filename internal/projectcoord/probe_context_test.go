package projectcoord

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStoreCancelledEnvironmentProbePreservesDurableRun(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	if output, err := exec.Command("git", "-C", workspace, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("prepare Git repository: %v: %s", err, output)
	}
	base := NewStore(root)
	run, err := base.Create(CreateInput{Workspace: workspace, Goal: "Keep prior state on an incomplete observation"})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Environment.IsGitRepository {
		t.Fatal("fixture did not persist its Git capability")
	}
	path, err := base.runPath(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewStore(root, WithProbeContext(ctx)).Get(workspace, run.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled service probe was persisted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cancelled observation changed durable bytes: %v", err)
	}
	// The child CLI opens an independent default Store; service cancellation must
	// not prevent its result/failure bookkeeping from reopening the same graph.
	if _, err := NewStore(root).Get(workspace, run.ID); err != nil {
		t.Fatalf("service cancellation leaked into independent worker Store: %v", err)
	}
}

func TestStoreProbeDeadlineDoesNotOverwritePriorRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX controlled Git process fixture")
	}
	workspace, root := t.TempDir(), t.TempDir()
	if output, err := exec.Command("git", "-C", workspace, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("prepare Git repository: %v: %s", err, output)
	}
	base := NewStore(root)
	run, err := base.Create(CreateInput{Workspace: workspace, Goal: "Keep prior state after Git timeout"})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Environment.IsGitRepository {
		t.Fatal("fixture did not persist its Git capability")
	}
	path, _ := base.runPath(workspace, run.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = NewStore(root, WithProbeContext(ctx)).Get(workspace, run.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Git timeout became a non-Git observation: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("Git timeout changed durable bytes: %v", err)
	}
	if strings.Contains(string(after), "task_deadline_exceeded") {
		t.Fatal("Probe deadline was confused with a task deadline")
	}
}
