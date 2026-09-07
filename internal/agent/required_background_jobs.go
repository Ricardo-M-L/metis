package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// finiteJobTracker retains ownership after notifications are consumed. The
// pending-await map only tracks wakeups; neither that map nor model prose is
// sufficient proof that required checks really exited successfully.
type finiteJobTracker struct {
	mu       sync.Mutex
	closed   bool
	pool     *jobs.Registry
	owned    map[string]*jobs.Registry
	required map[string]*jobs.Registry
}

type finiteJobTrackerContextKey struct{}

// StartFiniteBackgroundJob atomically starts and registers a finite Bash job
// under its owning Run. Registration must not depend on the tool_result reaching
// the loop: cancellation can discard that result after the process has started.
// The spawn closure must only start the process, not wait for its completion.
// Ordinary servers do not use this API and retain their existing lifecycle.
func StartFiniteBackgroundJob(ctx context.Context, pool *jobs.Registry, required bool, spawn func() (*jobs.Job, error)) (*jobs.Job, error) {
	tracker, _ := ctx.Value(finiteJobTrackerContextKey{}).(*finiteJobTracker)
	if tracker == nil {
		return spawn()
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.closed {
		return nil, context.Canceled
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	job, err := spawn()
	if err == nil && job != nil {
		tracker.recordOwnedLocked(pool, job.ID, required)
	}
	return job, err
}

func (t *finiteJobTracker) recordOwnedLocked(pool *jobs.Registry, id string, required bool) {
	if t.owned == nil {
		t.owned = make(map[string]*jobs.Registry)
	}
	if old, ok := t.owned[id]; !ok || old == nil {
		t.owned[id] = pool
	}
	if required {
		if t.required == nil {
			t.required = make(map[string]*jobs.Registry)
		}
		t.required[id] = t.owned[id]
	}
}

func (t *finiteJobTracker) record(results []llm.ContentBlock, pending map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, result := range results {
		if result.Type != "tool_result" || result.IsError || result.Presentation == nil {
			continue
		}
		id, _ := result.Presentation["job_id"].(string)
		await, _ := result.Presentation["await_completion"].(bool)
		required, _ := result.Presentation["required_for_completion"].(bool)
		if id == "" || (!await && !required) {
			continue
		}
		t.recordOwnedLocked(t.pool, id, required)
		pending[id] = struct{}{}
	}
}

func (t *finiteJobTracker) completionStatus() (reason, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range sortedJobIDs(t.required) {
		pool := t.required[id]
		if pool == nil {
			return "environment_blocked", "Required job registry is unavailable; completion cannot be established."
		}
		job, ok := pool.Get(id)
		if !ok {
			return "environment_blocked", "Required job is missing from the registry: " + id
		}
		if job.Status != jobs.StatusCompleted || job.ExitCode != 0 {
			return "acceptance_incomplete", fmt.Sprintf("Required job %s did not complete successfully: status=%s exit=%d", id, job.Status, job.ExitCode)
		}
	}
	return "", ""
}

// Cleanup only IDs started as finite jobs by this Run, never unrelated servers.
// StopAndWait joins the real process lifecycle, not just the status transition.
func (t *finiteJobTracker) cleanup(ctx context.Context) error {
	t.mu.Lock()
	t.closed = true
	owned := make(map[string]*jobs.Registry, len(t.owned))
	for id, owner := range t.owned {
		owned[id] = owner
	}
	t.mu.Unlock()
	ids := sortedJobIDs(owned)
	var cleanupErr error
	// Signal every owned job first. An unresponsive first join must not leave
	// later jobs running when the bounded cleanup context expires.
	for _, id := range ids {
		if owner := owned[id]; owner != nil {
			cleanupErr = errors.Join(cleanupErr, owner.Stop(id, 100*time.Millisecond))
		}
	}
	for _, id := range ids {
		if owner := owned[id]; owner != nil {
			cleanupErr = errors.Join(cleanupErr, owner.StopAndWait(ctx, id, 100*time.Millisecond))
		}
	}
	return cleanupErr
}

func sortedJobIDs[T any](ids map[string]T) []string {
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	return keys
}
