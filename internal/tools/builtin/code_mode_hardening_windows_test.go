//go:build windows

package builtin

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestStopRunCodeProcessDoesNotSynchronouslyWaitForTreeKiller(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCodeCancellationHelperProcess$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	previous := runCodeKillProcessTree
	blockTreeKill := make(chan struct{})
	t.Cleanup(func() {
		runCodeKillProcessTree = previous
		close(blockTreeKill)
	})
	runCodeKillProcessTree = func(*os.Process) { <-blockTreeKill }

	start := time.Now()
	stopRunCodeProcess(cmd, done, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("RunCode cancellation returned before its bound after %s", elapsed)
	} else if elapsed > time.Second {
		t.Fatalf("RunCode cancellation blocked on tree killer for %s", elapsed)
	}
}

func TestRunCodeCancellationHelperProcess(t *testing.T) {
	time.Sleep(30 * time.Second)
}
