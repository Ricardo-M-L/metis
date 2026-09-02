//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestRuntimeConcurrentSessionBoundaryCannotHideJobsFromFullAccessRevocation
// reproduces the listener/session-boundary ordering that originally lost a
// fullAccess process: the listener records the safer mode, releaseSessionWork
// and the listener's strict revoke then contend for the same jobs generation.
// The session boundary must itself join that generation; the listener may
// serialize behind it, but neither operation may return while a Registry-owned
// Wait path is still outstanding.
func TestRuntimeConcurrentSessionBoundaryCannotHideJobsFromFullAccessRevocation(t *testing.T) {
	jobRegistry := jobs.NewRegistry(t.TempDir())
	jobCtx, cancelJob := context.WithCancel(context.Background())
	out, _, err := jobRegistry.NewDiskOutput()
	if err != nil {
		cancelJob()
		t.Fatal(err)
	}

	command := exec.CommandContext(jobCtx, os.Args[0], "-test.run=^TestRuntimeLifecycleBlockingJobHelper$")
	command.Env = append(os.Environ(), "METIS_TEST_BLOCKING_LIFECYCLE_JOB=1")
	command.Stdout = out.Writer()
	command.Stderr = out.Writer()
	jobs.ApplyProcessGroup(command)
	if err := command.Start(); err != nil {
		cancelJob()
		jobRegistry.CleanupOrphan(out)
		t.Fatal(err)
	}

	// The OS wait completes normally, but Registry ownership is deliberately
	// held at the same handoff seam used by Bash auto-background promotion.
	// This makes the Reset -> strict ResetAndWait interleaving deterministic.
	rawWait := make(chan error, 1)
	go func() { rawWait <- command.Wait() }()
	leaderReaped := make(chan struct{})
	releaseRegistryWait := make(chan struct{})
	gatedWait := make(chan error, 1)
	go func() {
		waitErr := <-rawWait
		close(leaderReaped)
		<-releaseRegistryWait
		gatedWait <- waitErr
	}()

	var releaseOnce sync.Once
	releaseWait := func() { releaseOnce.Do(func() { close(releaseRegistryWait) }) }
	t.Cleanup(func() {
		releaseWait()
		cancelJob()
		jobRegistry.ResetAndWait(0)
	})

	job, err := jobRegistry.Adopt(jobs.AdoptArgs{
		Command:    "test-blocking-job",
		Cmd:        command,
		Cancel:     cancelJob,
		Output:     out,
		StartTime:  time.Now(),
		WaitResult: gatedWait,
	})
	if err != nil {
		releaseWait()
		cancelJob()
		jobRegistry.CleanupOrphan(out)
		t.Fatal(err)
	}

	rt := &runtime{
		loop:           &agent.Loop{Jobs: jobRegistry},
		permissionMode: permission.ModeFullAccess,
	}
	if !rt.recordPermissionModeChange(permission.ModeDefault) {
		t.Fatal("fullAccess -> default transition was not recorded as a revoke edge")
	}

	boundaryDone := make(chan struct{})
	go func() {
		rt.releaseSessionWork()
		close(boundaryDone)
	}()
	select {
	case <-leaderReaped:
	case <-time.After(3 * time.Second):
		t.Fatal("strict session boundary did not terminate and reap the helper leader")
	}
	if _, ok := jobRegistry.Get(job.ID); ok {
		t.Fatal("strict session boundary retained the old job in the public registry")
	}
	select {
	case <-boundaryDone:
		t.Fatal("strict session boundary returned before Registry Wait completed")
	default:
	}

	revoked := make(chan struct{})
	go func() {
		rt.revokeFullAccessResources()
		close(revoked)
	}()
	select {
	case <-revoked:
		t.Fatal("fullAccess revoke returned while the strict session boundary still owned the generation")
	case <-time.After(50 * time.Millisecond):
	}

	releaseWait()
	select {
	case <-boundaryDone:
	case <-time.After(3 * time.Second):
		t.Fatal("strict session boundary did not finish after Registry Wait completed")
	}
	select {
	case <-revoked:
	case <-time.After(3 * time.Second):
		t.Fatal("fullAccess revoke did not finish after the session boundary released the generation")
	}
	if command.ProcessState == nil {
		t.Fatal("strict session boundary returned before the command was reaped")
	}
}
