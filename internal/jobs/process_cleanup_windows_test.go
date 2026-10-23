//go:build windows

package jobs

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
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
	cleanup := NewProcessGroupCleaner(&exec.Cmd{Process: &os.Process{Pid: 123456}})
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
}
