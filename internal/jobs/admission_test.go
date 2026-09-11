package jobs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpawnAlreadyCancelledAdmissionDoesNotCreateResources(t *testing.T) {
	r := quickRegistry(t)
	t.Cleanup(func() { r.ResetAndWait(0) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := exec.Command("sh", "-c", "printf must-not-start")
	j, err := r.Spawn(SpawnArgs{AdmissionContext: ctx, Command: "must-not-start", Cmd: cmd})
	if !errors.Is(err, context.Canceled) || j != nil {
		t.Fatalf("cancelled admission returned job=%v err=%v", j, err)
	}
	if cmd.Process != nil || len(r.List()) != 0 {
		t.Error("already cancelled admission created a process or job")
	}
	if _, err := os.Stat(r.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("already cancelled admission created an output directory: %v", err)
	}
}

func TestSpawnAdmissionCancelledWhileWaitingForRegistry(t *testing.T) {
	r := quickRegistry(t)
	t.Cleanup(func() { r.ResetAndWait(0) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command("sh", "-c", "printf must-not-start")
	type outcome struct {
		job *Job
		err error
	}
	result := make(chan outcome, 1)
	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	go func() {
		j, err := r.Spawn(SpawnArgs{AdmissionContext: ctx, Command: "must-not-start", Cmd: cmd})
		result <- outcome{j, err}
	}()

	// Holding the admission lock makes the output file a deterministic
	// progress barrier: Spawn has prepared resources but cannot start Cmd.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		files, err := filepath.Glob(filepath.Join(r.dir, "*.out"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 1 {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("Spawn never reached prepared-output admission barrier")
		}
	}
	cancel()
	select {
	case got := <-result:
		if !errors.Is(got.err, context.Canceled) || got.job != nil {
			t.Errorf("cancelled admission returned job=%v err=%v", got.job, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled Spawn remained blocked on admission lock")
	}
	r.mu.Unlock()
	locked = false
	if cmd.Process != nil {
		t.Error("cancelled admission started a process")
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("cancelled admission registered %d jobs", got)
	}
	files, err := os.ReadDir(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("cancelled admission retained output files: %v", files)
	}
}

func TestSpawnAdmissionCancellationDoesNotCancelStartedDetachedJob(t *testing.T) {
	r := quickRegistry(t)
	t.Cleanup(func() { r.ResetAndWait(0) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = release.Close() })
	cmd := exec.Command("sh", "-c", "read line; printf 'completed:%s' \"$line\"")
	cmd.Stdin = input
	j, err := r.Spawn(SpawnArgs{AdmissionContext: ctx, Command: "detached service", Cmd: cmd})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.WriteString(release, "released\n"); err != nil {
		t.Fatal(err)
	}
	_ = release.Close()
	final := waitForStatus(t, r, j.ID, StatusCompleted, 3*time.Second)
	output, err := ReadJobOutput(final.OutputPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "completed:released") {
		t.Fatalf("detached job failed to finish after admission cancellation: %q", output)
	}
}
