package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func makeMailboxConfig(t *testing.T) MailboxConfig {
	t.Helper()
	dir := t.TempDir()
	return MailboxConfig{
		RunID:         "test-run",
		ToWorkerDir:   filepath.Join(dir, "to-worker"),
		ToCoordDir:    filepath.Join(dir, "to-coord"),
		PollInterval:  50 * time.Millisecond,
		WorkerTimeout: 2 * time.Second,
	}
}

// TestCoordinator_RoundTrip — coordinator dispatches, worker polls,
// processes, posts result; coordinator's AwaitResults collects it.
// Models the full dispatch → claim → reply → collect cycle without
// any LLM involvement.
func TestCoordinator_RoundTrip(t *testing.T) {
	cfg := makeMailboxConfig(t)

	// Coordinator side: dispatch one task.
	task := WorkerTask{ID: "t-1", Phase: "research", Prompt: "find usages of X"}
	if err := DispatchTask(cfg, task); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Worker side: poll, claim, reply.
	go func() {
		// Slight delay so AwaitResults has actually started polling.
		time.Sleep(100 * time.Millisecond)
		got, err := PollForTask(cfg)
		if err != nil || got == nil {
			t.Errorf("worker poll: err=%v got=%+v", err, got)
			return
		}
		_ = PostResult(cfg, WorkerResult{
			TaskID: got.ID,
			Phase:  got.Phase,
			OK:     true,
			Output: "found 3 usages",
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results, err := AwaitResults(ctx, cfg, []string{task.ID})
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	r, ok := results[task.ID]
	if !ok {
		t.Fatalf("missing result for %s", task.ID)
	}
	if r.Output != "found 3 usages" {
		t.Errorf("output mismatch: %q", r.Output)
	}
	if !r.OK {
		t.Errorf("OK should be true")
	}
}

// TestCoordinator_AwaitResultsTimeout — when no worker replies, await
// returns the partial results + a timeout error. The coordinator can
// then decide to retry, surface the failure, etc.
func TestCoordinator_AwaitResultsTimeout(t *testing.T) {
	cfg := makeMailboxConfig(t)
	cfg.WorkerTimeout = 200 * time.Millisecond
	_ = DispatchTask(cfg, WorkerTask{ID: "lonely", Phase: "research", Prompt: "x"})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	results, err := AwaitResults(ctx, cfg, []string{"lonely"})
	if err == nil {
		t.Errorf("expected timeout error; got nil")
	}
	if len(results) != 0 {
		t.Errorf("expected empty results on timeout; got %d", len(results))
	}
}

// TestCoordinator_PollForTaskFIFO — multiple pending tasks are polled
// in mtime order so daemon restarts don't reshuffle work.
func TestCoordinator_PollForTaskFIFO(t *testing.T) {
	cfg := makeMailboxConfig(t)

	// Dispatch 3 in deliberate order.
	for _, id := range []string{"a", "b", "c"} {
		_ = DispatchTask(cfg, WorkerTask{ID: id, Phase: "x", Prompt: id})
		time.Sleep(20 * time.Millisecond) // ensure mtime ordering
	}

	for _, want := range []string{"a", "b", "c"} {
		got, err := PollForTask(cfg)
		if err != nil {
			t.Fatalf("poll %s: %v", want, err)
		}
		if got == nil {
			t.Fatalf("poll %s returned nil; queue empty mid-test", want)
		}
		if got.ID != want {
			t.Errorf("FIFO violated: got %q want %q", got.ID, want)
		}
	}
}

// TestCoordinator_ConcurrentClaimsExclusive — two concurrent workers
// can't both claim the same task. Atomic-rename in PollForTask is what
// makes this work; if we used simple read-and-process the workers
// would duplicate work.
func TestCoordinator_ConcurrentClaimsExclusive(t *testing.T) {
	cfg := makeMailboxConfig(t)
	_ = DispatchTask(cfg, WorkerTask{ID: "race", Phase: "x", Prompt: "y"})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var claims []string
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ := PollForTask(cfg)
			if got != nil {
				mu.Lock()
				claims = append(claims, got.ID)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claims) != 1 {
		t.Errorf("exactly one worker should claim; got %d (%v)", len(claims), claims)
	}
}
