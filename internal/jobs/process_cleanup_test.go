package jobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestProcessGroupCleanupHelper(t *testing.T) {
	if os.Getenv("METIS_TEST_PROCESS_GROUP_CLEANUP_EXIT") == "1" {
		os.Exit(7)
	}
}

func TestProcessGroupCleanupAfterWaitPolicy(t *testing.T) {
	for _, unsuccessful := range []bool{false, true} {
		command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupCleanupHelper$")
		if unsuccessful {
			command.Env = append(os.Environ(), "METIS_TEST_PROCESS_GROUP_CLEANUP_EXIT=1")
		}
		waitErr := command.Run()
		if command.ProcessState == nil || !command.ProcessState.Exited() {
			t.Fatalf("helper was not reaped: %v", waitErr)
		}
		if unsuccessful != (waitErr != nil) {
			t.Fatalf("helper unsuccessful=%t: %v", unsuccessful, waitErr)
		}
		if needsProcessGroupCleanupAfterWait(command, waitErr, true) {
			t.Fatalf("completed Windows command unnecessarily requires taskkill: %v", waitErr)
		}
		if !needsProcessGroupCleanupAfterWait(command, waitErr, false) {
			t.Fatal("Unix completion lost descendant group cleanup")
		}
		if !needsProcessGroupCleanupAfterWait(command, errors.Join(waitErr, exec.ErrWaitDelay), true) {
			t.Fatal("incomplete Windows pipe join lost tree cleanup")
		}
	}
	for _, windows := range []bool{false, true} {
		if needsProcessGroupCleanupAfterWait(nil, nil, windows) || needsProcessGroupCleanupAfterWait(&exec.Cmd{}, nil, windows) {
			t.Fatal("unstarted command requires cleanup")
		}
		command := &exec.Cmd{Process: &os.Process{Pid: 123456}}
		if !needsProcessGroupCleanupAfterWait(command, context.Canceled, windows) {
			t.Fatal("unjoined cancelled command lost tree cleanup")
		}
	}
}
