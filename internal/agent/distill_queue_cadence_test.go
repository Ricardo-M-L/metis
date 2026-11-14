package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDurableDistillationCadenceReleasesEntireWindowAtNthTurn(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	provider := &memoryRepositoryE2EProvider{}
	loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory, loop.DistillEvery = manager, 5
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: snapshot.sessionID, WorkingDirectory: snapshot.workspacePath}
	}
	var launches int
	loop.DurableDistillationLauncher = func(string) error { launches++; return nil }
	for turn := 1; turn <= 5; turn++ {
		loop.AppendUser(fmt.Sprintf("Completed turn %d carries a durable acceptance preference for future work.", turn))
		if err := loop.Run(context.Background(), make(chan Event, 32)); err != nil {
			t.Fatal(err)
		}
		jobs, err := listDistillationQueueJobs(manager.Root())
		if err != nil || len(jobs) != turn {
			t.Fatalf("turn %d jobs=%v err=%v", turn, jobs, err)
		}
		for _, job := range jobs {
			if job.Ready != (turn == 5) || job.Status != "pending" || job.Attempts != 0 {
				t.Fatalf("turn %d cadence drift: %+v", turn, job)
			}
		}
		if turn < 5 && launches != 0 {
			t.Fatalf("turn %d started helper before cadence", turn)
		}
	}
	if launches != 1 || provider.distillRequestCount() != 0 {
		t.Fatalf("cadence launches=%d inline LLM=%d", launches, provider.distillRequestCount())
	}
	var consumed int
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return &durableQueueRepository{Repository: manager, process: func(context.Context, string, string) error { consumed++; return nil }}, provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	if consumed != 5 {
		t.Fatalf("Nth turn released %d exchanges, want entire five-turn window", consumed)
	}
}

func TestDurableDistillationActiveWorkerCannotConsumeStagedExchange(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var consumed atomic.Int32
	go func() {
		done <- RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
			return &durableQueueRepository{Repository: manager, process: func(ctx context.Context, _, _ string) error {
				if consumed.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}}, provider, nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ready worker did not enter")
	}
	snapshot.waitForCadence, snapshot.sourceMessageID, snapshot.sourceRequestID = true, "still-before-cadence", "staged-request"
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not leave staged work alone")
	}
	if consumed.Load() != 1 {
		t.Fatalf("already-running worker consumed staged exchange: %d", consumed.Load())
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.SourceMessageID == snapshot.sourceMessageID && (job.Ready || job.Attempts != 0 || job.Status != "pending") {
			t.Fatalf("staged exchange admitted: %+v", job)
		}
	}
}

func TestDurableDistillationRecoveryPreservesLiveOwnerAndAllowsExplicitBoundary(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	snapshot.waitForCadence = true
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.sourceMessageID = "legacy-ownerless-staged"
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	for _, job := range jobs {
		if job.SourceMessageID == snapshot.sourceMessageID {
			job.OwnerPID = 0
			if err := writeDistillationQueueJob(manager.Root(), job); err != nil {
				t.Fatal(err)
			}
		}
	}
	if released, err := RecoverDurableDistillationQueue(manager.Root()); err != nil || released != 1 {
		t.Fatalf("startup recovery=%d,%v, want only ownerless job", released, err)
	}
	jobs, _ = listDistillationQueueJobs(manager.Root())
	for _, job := range jobs {
		if job.OwnerPID == os.Getpid() && job.Ready {
			t.Fatalf("new startup released another live foreground: %+v", job)
		}
		if job.OwnerPID == 0 && !job.Ready {
			t.Fatalf("legacy job could not recover: %+v", job)
		}
	}
	if released, err := ReleaseDurableDistillationQueue(manager.Root(), snapshot.sessionID); err != nil || released != 1 {
		t.Fatalf("explicit boundary could not release live owner: %d,%v", released, err)
	}
}

func TestDurableDistillationFailedTurnDoesNotReleaseStagedExchange(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	snapshot.waitForCadence = true
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = manager
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: snapshot.sessionID, WorkingDirectory: snapshot.workspacePath}
	}
	var launches int
	loop.DurableDistillationLauncher = func(string) error { launches++; return nil }
	loop.AppendUser("This new request fails before a successful final answer can be recorded.")
	if err := loop.Run(context.Background(), make(chan Event, 32)); err == nil {
		t.Fatal("failed stream unexpectedly completed")
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 || jobs[0].Ready || jobs[0].Attempts != 0 || launches != 0 {
		t.Fatalf("failed turn released prior work: jobs=%+v launches=%d err=%v", jobs, launches, err)
	}
}

func TestDurableDistillationDeletionScrubsStagedReadyAndRunningJobs(t *testing.T) {
	manager, _, snapshot := queueFixture(t)
	for i := range 3 {
		snapshot.sourceMessageID = fmt.Sprintf("queued-state-%d", i)
		snapshot.waitForCadence = i == 0
		if _, err := enqueueDurableDistillation(snapshot); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := listDistillationQueueJobs(manager.Root())
	for _, job := range jobs {
		if job.SourceMessageID == "queued-state-2" {
			job.Status = "running"
			if err := writeDistillationQueueJob(manager.Root(), job); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := manager.DeleteSession(snapshot.sessionID); err != nil {
		t.Fatal(err)
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 3 {
		t.Fatalf("delete queue=%+v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if job.Status != "revoked" || job.UserMessage != "" || job.AssistantMessage != "" {
			t.Fatalf("delete retained staged/ready/running payload: %+v", job)
		}
	}
}

func TestDurableDistillationKilledForegroundRecoversStagedPending(t *testing.T) {
	manager, provider, snapshot := queueFixture(t)
	marker := filepath.Join(t.TempDir(), "staged-on-disk")
	child := exec.Command(os.Args[0], "-test.run=^TestDurableDistillationStagedForegroundHelper$")
	child.Env = append(os.Environ(), "METIS_STAGED_FOREGROUND_HELPER=1", "METIS_QUEUE_KILL_ROOT="+manager.Root(), "METIS_QUEUE_KILL_WORKSPACE="+snapshot.workspacePath, "METIS_QUEUE_KILL_MARKER="+marker)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground did not durably stage exchange")
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, err := listDistillationQueueJobs(manager.Root())
	if err != nil || len(jobs) != 1 || jobs[0].Ready || jobs[0].OwnerPID != child.Process.Pid {
		t.Fatalf("staged child identity=%+v err=%v", jobs, err)
	}
	if released, err := RecoverDurableDistillationQueue(manager.Root()); err != nil || released != 0 {
		t.Fatalf("live foreground released early=%d,%v", released, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if released, err := RecoverDurableDistillationQueue(manager.Root()); err != nil || released != 1 {
		t.Fatalf("dead foreground not recovered=%d,%v", released, err)
	}
	var consumed int
	if err := RunDurableDistillationQueue(context.Background(), manager.Root(), func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return &durableQueueRepository{Repository: manager, process: func(context.Context, string, string) error { consumed++; return nil }}, provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	if consumed != 1 {
		t.Fatalf("crashed pending exchange consumed %d times", consumed)
	}
}

func TestDurableDistillationStagedForegroundHelper(t *testing.T) {
	if os.Getenv("METIS_STAGED_FOREGROUND_HELPER") != "1" {
		return
	}
	manager, err := memory.NewMemoryManagerForWorkspace(os.Getenv("METIS_QUEUE_KILL_ROOT"), os.Getenv("METIS_QUEUE_KILL_WORKSPACE"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := distillSnapshot{repository: manager, provider: &durableQueueProvider{}, sessionID: "killed-pending-session", sourceMessageID: "killed-pending-message", sourceRequestID: "killed-pending-request", workspacePath: os.Getenv("METIS_QUEUE_KILL_WORKSPACE"), userMsg: "Preserve this real staged preference across an abrupt foreground exit.", assistantMsg: "The exchange is recorded before the process is deliberately killed.", waitForCadence: true}
	if _, err := enqueueDurableDistillation(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("METIS_QUEUE_KILL_MARKER"), []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(time.Minute):
		t.Fatal(errors.New("foreground kill test did not terminate helper"))
	}
}
