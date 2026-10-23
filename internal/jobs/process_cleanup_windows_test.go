//go:build windows

package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessGroupCleanerSkipsTaskkillForReapedWindowsCommand(t *testing.T) {
	previous := taskkillCommandContext
	t.Cleanup(func() { taskkillCommandContext = previous })
	var calls atomic.Int32
	taskkillCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls.Add(1)
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessGroupCleanupHelper$")
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupCleanupHelper$")
	cleanup := NewProcessGroupCleaner(command)
	err := command.Run()
	if err != nil {
		t.Fatal(err)
	}
	cleanup.AfterWait(err)
	if calls.Load() != 0 {
		t.Fatalf("normal completed command spawned taskkill %d times", calls.Load())
	}
}

func TestProcessGroupCleanerJoinsOneWindowsTaskkill(t *testing.T) {
	previous := taskkillCommandContext
	t.Cleanup(func() { taskkillCommandContext = previous })
	var calls atomic.Int32
	taskkillCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls.Add(1)
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessGroupCleanupHelper$")
	}
	_, cleanup, done := startProcessGroupCleanerWindowsHelper(t)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			cleanup.Kill()
			cleanup.AfterWait(exec.ErrWaitDelay)
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cancel and incomplete join spawned taskkill %d times", calls.Load())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("taskkill stub succeeded without stopping the owned leader")
	}
}

func TestProcessGroupCleanerWindowsHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--" || i+1 >= len(os.Args) {
			continue
		}
		switch os.Args[i+1] {
		case "wait":
			if i+2 >= len(os.Args) || os.WriteFile(os.Args[i+2], []byte("ready"), 0600) != nil {
				os.Exit(2)
			}
			time.Sleep(30 * time.Second)
		case "taskkill-failure":
			os.Exit(7)
		case "taskkill-timeout":
			time.Sleep(30 * time.Second)
		}
		return
	}
}

func startProcessGroupCleanerWindowsHelper(t *testing.T) (*exec.Cmd, *ProcessGroupCleaner, <-chan struct{}) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupCleanerWindowsHelper$", "--", "wait", ready)
	cleanup := NewProcessGroupCleaner(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		err := command.Wait()
		cleanup.AfterWait(err)
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned helper was not reaped during test cleanup")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return command, cleanup, done
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned helper did not become ready")
	return nil, nil, nil
}

func TestProcessGroupCleanerWindowsFallbackReapsLeaderAfterTaskkillFailure(t *testing.T) {
	for _, mode := range []string{"taskkill-failure", "taskkill-timeout"} {
		t.Run(mode, func(t *testing.T) {
			previous := taskkillCommandContext
			t.Cleanup(func() { taskkillCommandContext = previous })
			var calls atomic.Int32
			taskkillCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				calls.Add(1)
				return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessGroupCleanerWindowsHelper$", "--", mode)
			}
			command, cleanup, done := startProcessGroupCleanerWindowsHelper(t)
			started := time.Now()
			cleanup.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("failed taskkill left the owned leader alive and Wait blocked")
			}
			if elapsed := time.Since(started); elapsed > windowsTaskkillLimit+3*time.Second {
				t.Fatalf("failed taskkill cleanup exceeded its bound: %s", elapsed)
			}
			if command.ProcessState == nil || !command.ProcessState.Exited() {
				t.Fatal("owned leader was not reaped")
			}
			cleanup.Kill()
			cleanup.AfterWait(exec.ErrWaitDelay)
			if calls.Load() != 1 {
				t.Fatalf("failure cleanup launched taskkill %d times", calls.Load())
			}
		})
	}
}
