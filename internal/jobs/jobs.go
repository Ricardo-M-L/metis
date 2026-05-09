// Package jobs implements the Bash auto-background "job pool" — long-
// running shell commands that the foreground turn no longer waits for.
// Mirrors claude-code's tasks/LocalShellTask layout
// (services + TaskList/TaskOutput/TaskStop tools), with one practical
// adaptation: metis fires the auto-background timer at 60s instead of
// claude-code's 15s. 15s is right for a 1P assistant where the model
// has rich job-pool tools to spawn alongside; on metis with our
// model+tooling mix, 15s misfires too often on commands that just need
// a few extra seconds (npm install, golang test ./...). 60s is the
// crush-style sweet spot.
//
// Layout:
//
//	jobs.go        — Job state machine + Registry + notify channel
//	output.go      — disk-backed stdout/stderr writer (~/.metis/jobs/<id>.out)
//	id.go          — short hex IDs (`bg_<8hex>`)
//
// Why a fresh package and not extending internal/tasks/: tasks holds
// per-session todo lists (TodoWrite/TodoRead). Two unrelated concepts
// sharing a name causes lookup confusion; jobs is the bash-pool side
// only. Cross-package boundary keeps both lean.
package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Status is the lifecycle stage of a Job. Changes are monotonic except
// for Killed which can come in from any non-terminal state.
type Status int

const (
	// StatusRunning — process spawned, command active, output growing.
	StatusRunning Status = iota
	// StatusCompleted — process exited 0.
	StatusCompleted
	// StatusFailed — process exited non-zero (or hit signal that wasn't
	// our SIGTERM/SIGKILL).
	StatusFailed
	// StatusKilled — JobStop tool or registry shutdown sent SIGTERM/
	// SIGKILL. Distinct from StatusFailed because the model should
	// know its kill request landed (not pretend it got a real exit).
	StatusKilled
)

// String renders Status for prompt-facing output (JobList tool result).
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusKilled:
		return "killed"
	}
	return "unknown"
}

// Job is one background shell command. Fields are read with
// Registry.mu held; mutators (state transitions) lock too. The
// `cmd` / `cancel` / `output` triple is owned: Registry creates them
// in Spawn and never hands them out raw — JobOutput / JobStop go
// through Registry methods that take the lock.
type Job struct {
	ID          string
	Command     string // raw shell line as the model wrote it
	Description string // ≤80-char one-liner derived from Command, for JobList
	Status      Status
	StartTime   time.Time
	EndTime     time.Time // zero until terminal
	ExitCode    int       // -1 until exited

	OutputPath string // absolute path to ~/.metis/jobs/<id>.out

	// Internal: live process handle + cancel root for both auto- and
	// user-triggered termination paths. Held for the lifetime of the
	// Job; cleared on transition out of StatusRunning so the GC can
	// reap when Registry forgets the entry.
	cmd    *exec.Cmd
	cancel context.CancelFunc
	output *DiskOutput
}

// Notification is published on Registry.Notify when a job leaves
// StatusRunning. Consumers (the agent loop) drain this channel at
// turn boundaries and inject a `<job_notification>` system-reminder
// into the next prompt so the model knows its background work
// finished. Mirrors claude-code's <task_notification> envelope.
type Notification struct {
	JobID    string
	Status   Status
	ExitCode int
	Elapsed  time.Duration
	// Command echoed back so the model can correlate without a
	// JobList round-trip — most useful when many jobs are in flight.
	Command string
}

// Registry is the process-wide job pool. One instance lives in
// agent.Loop; tools (JobList/JobOutput/JobStop) and bash.go's
// auto-background path go through it.
//
// Thread safety: every public method takes mu (RWMutex). Spawn and
// state transitions take a write lock; reads (List / Get / Output)
// take read locks.
type Registry struct {
	mu     sync.RWMutex
	jobs   map[string]*Job
	notify chan Notification // buffered, drained by agent loop

	// dir overrides ~/.metis/jobs (used by tests).
	dir string
}

// NewRegistry constructs an empty pool whose disk dir is
// `<dir>/jobs/` — passing an empty string falls back to METIS_HOME or
// ~/.metis. Notify chan is buffered for 64 entries; tests that don't
// drain it should set buf via NewRegistryBuffered.
func NewRegistry(dir string) *Registry {
	return NewRegistryBuffered(dir, 64)
}

// NewRegistryBuffered exposes the notify-chan buffer size for tests
// that race many jobs simultaneously.
func NewRegistryBuffered(dir string, notifyBuf int) *Registry {
	if dir == "" {
		dir = home()
	}
	return &Registry{
		jobs:   make(map[string]*Job),
		notify: make(chan Notification, notifyBuf),
		dir:    filepath.Join(dir, "jobs"),
	}
}

// Notify returns the channel publishers can subscribe to. Receive-only
// — Registry owns the send side and closes it on Shutdown.
func (r *Registry) Notify() <-chan Notification {
	return r.notify
}

// home resolves the metis data root. Mirrors internal/auth's home()
// rule so we don't add a config-package dep just for this.
func home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".metis")
	}
	return "."
}

// SpawnArgs bundles everything Spawn needs from the caller. Pulled out
// so bash.go can build it once and pass through, instead of growing a
// 7-arg signature.
type SpawnArgs struct {
	// Command is the shell line to run (already passes safe-allowlist
	// or permission gate by the time we get here).
	Command string
	// Description is a short label shown in JobList. Empty → derived
	// from the first 80 chars of Command.
	Description string
	// Cmd is the constructed *exec.Cmd. Caller is responsible for
	// setting cwd / env / Setpgid; Spawn only attaches stdout/stderr
	// + records the cancel.
	Cmd *exec.Cmd
	// Cancel kills Cmd. Spawn calls it on JobStop / shutdown.
	Cancel context.CancelFunc
}

// AdoptArgs lets bash.go promote an already-running foreground cmd
// into a background job — used when the 60s auto-background timer
// fires. The cmd is alive, the output file is already open and being
// written to; Adopt just registers the entry and starts a wait
// goroutine. Critically, the process is NOT restarted, so any work
// already done isn't lost.
type AdoptArgs struct {
	Command     string
	Description string
	// Cmd must be already running (cmd.Start() called by the caller).
	Cmd *exec.Cmd
	// Cancel cancels the context root that backs cmd. Used by
	// JobStop to escalate to SIGKILL on grace timeout.
	Cancel context.CancelFunc
	// Output is the DiskOutput already capturing cmd.Stdout/Stderr.
	// Must be open; Registry takes ownership and Closes on Wait.
	Output *DiskOutput
	// StartTime is when the original foreground exec began. Preserved
	// so the model sees the true wall-clock duration, not the moment
	// of background-promotion.
	StartTime time.Time
}

// Spawn registers a new job, opens its output file, and starts the
// process. The job appears in List() immediately; the goroutine that
// waits on the process posts a Notification when it exits.
//
// Concurrent calls are safe: Spawn uses a write lock to insert into
// the map and assign a unique ID.
func (r *Registry) Spawn(a SpawnArgs) (*Job, error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: mkdir %s: %w", r.dir, err)
	}
	id := newJobID()
	outputPath := filepath.Join(r.dir, id+".out")
	out, err := newDiskOutput(outputPath)
	if err != nil {
		return nil, fmt.Errorf("jobs: open output %s: %w", outputPath, err)
	}

	desc := a.Description
	if desc == "" {
		desc = shortDesc(a.Command)
	}

	a.Cmd.Stdout = out.Writer()
	a.Cmd.Stderr = out.Writer()
	// Group the child + its descendants under their own pgid so
	// Stop's tree-kill can reach grandchildren (sleep & in `bash -c
	// 'sleep 60 & sleep 60 & wait'`). No-op on Windows.
	ApplyProcessGroup(a.Cmd)

	if err := a.Cmd.Start(); err != nil {
		_ = out.Close()
		return nil, fmt.Errorf("jobs: start: %w", err)
	}

	j := &Job{
		ID:          id,
		Command:     a.Command,
		Description: desc,
		Status:      StatusRunning,
		StartTime:   time.Now(),
		ExitCode:    -1,
		OutputPath:  outputPath,
		cmd:         a.Cmd,
		cancel:      a.Cancel,
		output:      out,
	}

	r.mu.Lock()
	r.jobs[id] = j
	r.mu.Unlock()

	// Wait goroutine: blocks on Wait, transitions state, publishes
	// Notification. Outside the registry lock so JobOutput readers
	// can hit the disk file mid-run without serialising on r.mu.
	go r.waitAndComplete(j)

	return j, nil
}

// Adopt registers an already-running foreground cmd as a background
// job. Used by bash.go when the 60s auto-background timer fires: the
// command keeps executing, but the model gets a job ID and stops
// waiting in the foreground turn.
//
// Critically: this does NOT call cmd.Start (it's already started).
// The wait goroutine that posts Notification is started inside
// Adopt, same as Spawn. Output ownership transfers to the Registry.
func (r *Registry) Adopt(a AdoptArgs) (*Job, error) {
	if a.Cmd == nil {
		return nil, fmt.Errorf("jobs: Adopt requires a non-nil Cmd")
	}
	if a.Output == nil {
		return nil, fmt.Errorf("jobs: Adopt requires an open Output")
	}
	id := newJobID()
	desc := a.Description
	if desc == "" {
		desc = shortDesc(a.Command)
	}
	startTime := a.StartTime
	if startTime.IsZero() {
		startTime = time.Now()
	}

	j := &Job{
		ID:          id,
		Command:     a.Command,
		Description: desc,
		Status:      StatusRunning,
		StartTime:   startTime,
		ExitCode:    -1,
		OutputPath:  a.Output.Path(),
		cmd:         a.Cmd,
		cancel:      a.Cancel,
		output:      a.Output,
	}

	r.mu.Lock()
	r.jobs[id] = j
	r.mu.Unlock()

	go r.waitAndComplete(j)

	return j, nil
}

// NewDiskOutput exposes the disk-file constructor so bash.go can
// open one before deciding whether to keep the run foreground or
// hand it off to Adopt. Path lives under the registry's jobs dir
// for consistency, even when the job is ultimately forgotten.
func (r *Registry) NewDiskOutput() (*DiskOutput, string, error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("jobs: mkdir %s: %w", r.dir, err)
	}
	id := newJobID()
	path := filepath.Join(r.dir, id+".out")
	out, err := newDiskOutput(path)
	if err != nil {
		return nil, "", err
	}
	return out, path, nil
}

// CleanupOrphan deletes a disk file produced by NewDiskOutput when
// the foreground caller decided not to Adopt (the job finished
// within the 60s budget, so there's no need to keep its log).
func (r *Registry) CleanupOrphan(out *DiskOutput) {
	if out == nil {
		return
	}
	path := out.Path()
	_ = out.Close()
	_ = os.Remove(path)
}

func (r *Registry) waitAndComplete(j *Job) {
	err := j.cmd.Wait()
	r.mu.Lock()
	if j.Status == StatusKilled {
		// JobStop already set the terminal state. Don't overwrite
		// (overwriting would race with the kill notifier).
		_ = j.output.Close()
		j.cmd = nil
		j.cancel = nil
		j.output = nil
		r.mu.Unlock()
		return
	}
	j.EndTime = time.Now()
	if err == nil {
		j.Status = StatusCompleted
		j.ExitCode = 0
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		j.Status = StatusFailed
		j.ExitCode = exitErr.ExitCode()
	} else {
		j.Status = StatusFailed
		j.ExitCode = -1
	}
	_ = j.output.Close()
	j.cmd = nil
	j.cancel = nil
	j.output = nil
	notif := Notification{
		JobID:    j.ID,
		Status:   j.Status,
		ExitCode: j.ExitCode,
		Elapsed:  j.EndTime.Sub(j.StartTime),
		Command:  j.Command,
	}
	r.mu.Unlock()

	// Non-blocking publish: if no one drains, drop. This keeps a
	// runaway "job spam" from deadlocking the wait goroutine. The
	// model can still see the completed status via JobList/JobOutput.
	select {
	case r.notify <- notif:
	default:
	}
}

// List returns a snapshot of all jobs sorted by StartTime ascending.
// Caller may store / mutate the slice but not the *Job entries (still
// owned by Registry). For prompt-facing rendering use the JobList
// tool which builds its own JSON-serializable view.
func (r *Registry) List() []*Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	// Stable order: oldest first. List() in claude-code returns the
	// same order; the model relies on it to find "the most recent
	// background task" without a sort step.
	for i := 0; i < len(out); i++ {
		for k := i + 1; k < len(out); k++ {
			if out[k].StartTime.Before(out[i].StartTime) {
				out[i], out[k] = out[k], out[i]
			}
		}
	}
	return out
}

// Get returns the Job with the given ID, or nil if unknown.
func (r *Registry) Get(id string) *Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.jobs[id]
}

// Stop terminates a job using a two-stage tree-kill: SIGTERM the
// process group, wait `grace` for cooperative cleanup, then SIGKILL
// anything still alive. Returns an error only if the job ID is
// unknown — failed signal delivery on an already-exited process is
// fine and reported as success.
//
// Why "tree-kill" matters here: a `bash -c 'do-stuff & wait'` spawns
// grandchildren that the bash leader doesn't forward signals to. A
// plain SIGTERM-the-leader leaves them as orphans (re-parented to
// init, still consuming CPU/network until they finish on their own).
// Sending to -pid hits the entire group at once, mirroring openclaw's
// kill-tree.ts — see /Users/ricardo/Documents/公司学习文件/opensource-contributions/openclaw/src/process/kill-tree.ts:54.
//
// `grace == 0` collapses to "SIGTERM then SIGKILL immediately" which
// matches claude-code's quick-kill stance for a few specific call
// sites (registry shutdown).
//
// Updates Status to StatusKilled and posts a Notification immediately,
// even though the wait goroutine may still be running (it'll observe
// Status=Killed and skip its own state mutation).
func (r *Registry) Stop(id string, grace time.Duration) error {
	r.mu.Lock()
	j, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("jobs: unknown id %q", id)
	}
	if j.Status != StatusRunning {
		r.mu.Unlock()
		return nil // already terminal — no-op
	}
	cmd := j.cmd
	cancel := j.cancel
	j.Status = StatusKilled
	j.EndTime = time.Now()
	j.ExitCode = -1
	command := j.Command
	elapsed := j.EndTime.Sub(j.StartTime)
	r.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		// Two-stage tree kill. We deliberately don't wait for the
		// goroutine here — the caller (BashKill tool) wants to return
		// fast, and the wait goroutine watching cmd.Wait will observe
		// the SIGKILL exit and clean up its own state.
		killTreeStaged(cmd.Process, grace, nil)
		// Belt-and-suspenders: cancel the context root too. If
		// SIGKILL-to-group failed for some platform-specific reason,
		// context cancellation drops cmd.Wait via the runtime's own
		// kill path and we still get a clean state transition.
		if cancel != nil {
			go func() {
				time.Sleep(grace + 500*time.Millisecond)
				cancel()
			}()
		}
	}

	select {
	case r.notify <- Notification{
		JobID:    id,
		Status:   StatusKilled,
		ExitCode: -1,
		Elapsed:  elapsed,
		Command:  command,
	}:
	default:
	}
	return nil
}

// Shutdown closes the notify channel and best-effort kills every
// running job. Called from agent.Loop teardown so a metis-quit
// doesn't orphan ~/.metis/jobs/<id>.out files for processes that are
// still writing.
func (r *Registry) Shutdown(grace time.Duration) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.jobs))
	for id, j := range r.jobs {
		if j.Status == StatusRunning {
			ids = append(ids, id)
		}
	}
	r.mu.RUnlock()
	for _, id := range ids {
		_ = r.Stop(id, grace)
	}
	// Don't close r.notify here — receivers may still be draining
	// the buffer post-Shutdown. Channel will be GC'd when the agent
	// loop exits.
}

// shortDesc trims a command to a single-line ≤80-char preview for
// JobList rendering. Drops trailing whitespace, replaces newlines
// with `⏎` so multi-line heredocs still render as one row.
func shortDesc(cmd string) string {
	const max = 80
	out := make([]rune, 0, max)
	for _, r := range cmd {
		if len(out) >= max {
			out = append(out, '…')
			break
		}
		switch r {
		case '\n', '\r':
			out = append(out, '⏎')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
