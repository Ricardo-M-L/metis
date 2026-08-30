//go:build windows

package jobs

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRunTaskkillWithLimitBoundsHungHelper(t *testing.T) {
	previous := taskkillCommandContext
	t.Cleanup(func() { taskkillCommandContext = previous })
	taskkillCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTaskkillHelperProcess$")
	}

	start := time.Now()
	err := runTaskkillWithLimit([]string{"/F", "/T", "/PID", "123"}, 40*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung taskkill helper unexpectedly succeeded")
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("taskkill returned before its deadline after %s", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("taskkill exceeded its hard bound: %s", elapsed)
	}
}

func TestTaskkillHelperProcess(t *testing.T) {
	time.Sleep(30 * time.Second)
}
