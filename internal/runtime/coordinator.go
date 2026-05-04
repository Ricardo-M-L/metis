// Package runtime — COORDINATOR_MODE multi-agent orchestration.
//
// claude-code's COORDINATOR_MODE feature flag (per leaked sourcemap)
// fans out a complex task into 4 phases:
//
//  1. Research      — N worker agents survey the codebase in parallel
//  2. Synthesis     — coordinator reads all worker reports, builds plan
//  3. Implementation — workers execute the plan in parallel
//  4. Verification  — workers run tests / smoke checks
//
// Communication uses utils/mailbox.ts (file-based queue between
// coordinator and workers).
//
// metis minimum-viable: file-based mailbox under
// ~/.metis/coordinator/<runID>/{coord-to-worker,worker-to-coord}/, with
// the existing Agent tool reused as the worker spawner. The
// coordinator is a single agent.Loop turn that dispatches research →
// synthesizes → dispatches implementation → verifies — each "dispatch"
// step writes task files into the worker mailboxes, then waits for
// reply files.
//
// Honest scope: we don't reimplement CC's full COORDINATOR_MODE. We
// provide the file-based mailbox primitives + a one-shot coordinator
// driver. A real workflow author wires the 4 phases on top.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MailboxRoot returns the base path for a coordinator run. Each runID
// gets its own subtree so concurrent coordinators don't collide.
func MailboxRoot(runID string) string {
	return filepath.Join(Home(), "coordinator", runID)
}

// MailboxConfig describes a file-based message queue between a
// coordinator and N workers. The coordinator writes into ToWorkerDir
// (one file per task), workers atomically rename their replies into
// ToCoordDir, the coordinator scans for them.
type MailboxConfig struct {
	RunID         string
	ToWorkerDir   string // coordinator → worker
	ToCoordDir    string // worker → coordinator
	PollInterval  time.Duration
	WorkerTimeout time.Duration
}

// DefaultMailboxConfig fills in standard paths under
// ~/.metis/coordinator/<runID>/.
func DefaultMailboxConfig(runID string) MailboxConfig {
	root := MailboxRoot(runID)
	return MailboxConfig{
		RunID:         runID,
		ToWorkerDir:   filepath.Join(root, "coord-to-worker"),
		ToCoordDir:    filepath.Join(root, "worker-to-coord"),
		PollInterval:  500 * time.Millisecond,
		WorkerTimeout: 5 * time.Minute,
	}
}

// EnsureMailbox creates the queue directories. Idempotent.
func EnsureMailbox(cfg MailboxConfig) error {
	for _, d := range []string{cfg.ToWorkerDir, cfg.ToCoordDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// WorkerTask is one unit of work the coordinator dispatches.
type WorkerTask struct {
	ID     string `json:"id"`
	Phase  string `json:"phase"`           // "research" | "implementation" | "verification"
	Prompt string `json:"prompt"`          // what the worker should do
	Input  string `json:"input,omitempty"` // optional structured payload
}

// WorkerResult is the worker's reply. Workers write this to ToCoordDir/<task.ID>.json.
type WorkerResult struct {
	TaskID    string `json:"task_id"`
	Phase     string `json:"phase"`
	OK        bool   `json:"ok"`
	Output    string `json:"output"`
	Error     string `json:"error,omitempty"`
	Duration  string `json:"duration"`
	Completed string `json:"completed_at"`
}

// DispatchTask writes a task file into ToWorkerDir. Workers (separate
// processes / goroutines / metis daemon instances) pick it up and
// reply into ToCoordDir.
func DispatchTask(cfg MailboxConfig, task WorkerTask) error {
	if err := EnsureMailbox(cfg); err != nil {
		return err
	}
	if task.ID == "" {
		task.ID = fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	b, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.ToWorkerDir, task.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic visibility
}

// AwaitResults polls ToCoordDir for replies matching the given task IDs.
// Returns when all expected results are in OR ctx is canceled OR the
// per-task timeout elapses (returns whatever's collected so far + a
// "timeout: missing N" error). Each result file is consumed (renamed
// to .processed/) so future calls don't re-collect.
func AwaitResults(ctx context.Context, cfg MailboxConfig, expectIDs []string) (map[string]WorkerResult, error) {
	if err := EnsureMailbox(cfg); err != nil {
		return nil, err
	}
	processedDir := filepath.Join(cfg.ToCoordDir, ".processed")
	_ = os.MkdirAll(processedDir, 0o755)

	want := make(map[string]bool, len(expectIDs))
	for _, id := range expectIDs {
		want[id] = true
	}
	got := map[string]WorkerResult{}
	deadline := time.Now().Add(cfg.WorkerTimeout)

	tick := time.NewTicker(cfg.PollInterval)
	defer tick.Stop()
	for {
		entries, err := os.ReadDir(cfg.ToCoordDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				path := filepath.Join(cfg.ToCoordDir, e.Name())
				body, rerr := os.ReadFile(path)
				if rerr != nil {
					continue
				}
				var r WorkerResult
				if jerr := json.Unmarshal(body, &r); jerr != nil {
					continue
				}
				if !want[r.TaskID] {
					continue
				}
				got[r.TaskID] = r
				_ = os.Rename(path, filepath.Join(processedDir, e.Name()))
			}
		}
		if len(got) == len(want) {
			return got, nil
		}
		if time.Now().After(deadline) {
			return got, fmt.Errorf("coordinator: timeout waiting for %d worker replies; got %d/%d", len(want)-len(got), len(got), len(want))
		}
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-tick.C:
		}
	}
}

// PollForTask is the worker side: scans ToWorkerDir for the next
// pending task (oldest first), reads it, atomically marks it claimed
// (rename to .claimed/<id>.json) so concurrent workers don't double-
// process. Returns nil, nil when the queue is empty.
func PollForTask(cfg MailboxConfig) (*WorkerTask, error) {
	if err := EnsureMailbox(cfg); err != nil {
		return nil, err
	}
	claimedDir := filepath.Join(cfg.ToWorkerDir, ".claimed")
	_ = os.MkdirAll(claimedDir, 0o755)

	entries, err := os.ReadDir(cfg.ToWorkerDir)
	if err != nil {
		return nil, err
	}
	type ent struct {
		path string
		mod  time.Time
	}
	var pending []ent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		pending = append(pending, ent{path: filepath.Join(cfg.ToWorkerDir, e.Name()), mod: info.ModTime()})
	}
	if len(pending) == 0 {
		return nil, nil
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].mod.Before(pending[j].mod) })

	body, err := os.ReadFile(pending[0].path)
	if err != nil {
		return nil, err
	}
	var task WorkerTask
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, err
	}
	// Claim atomically — rename out of the queue.
	claimed := filepath.Join(claimedDir, filepath.Base(pending[0].path))
	if err := os.Rename(pending[0].path, claimed); err != nil {
		return nil, err
	}
	return &task, nil
}

// PostResult is the worker side: writes a WorkerResult into the
// to-coordinator mailbox. Atomic via tmp+rename.
func PostResult(cfg MailboxConfig, r WorkerResult) error {
	if err := EnsureMailbox(cfg); err != nil {
		return err
	}
	if r.Completed == "" {
		r.Completed = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.ToCoordDir, r.TaskID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
