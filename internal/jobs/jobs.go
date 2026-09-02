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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/security"
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
	cmd     *exec.Cmd
	process *os.Process
	cancel  context.CancelFunc
	output  *DiskOutput
	// done closes only after the Registry-owned cmd.Wait path has reaped the
	// leader and every registered tree-kill stage has finished. ResetAndWait
	// uses this lifecycle edge instead of treating either signal delivery or
	// leader exit as proof that all descendants are gone.
	done       chan struct{}
	leaderDone bool
	doneClosed bool
	// killStages contains both running stages and stages registered under r.mu
	// but not started yet. A reset installs its replacement before cancelling
	// older stages, so there is never a zero-stage window while a process tree
	// can still outlive the session boundary.
	killStages map[*killStage]struct{}
	// generation identifies the top-level session that registered the job.
	// It is used only to suppress a completion notification that races a
	// Registry.Reset boundary; public snapshots intentionally omit it.
	generation uint64
}

// killStage has a cancellation latch because Stop can publish the stage under
// Registry.mu, then race a Reset that supersedes it before killTreeStaged has
// returned its cancellation function. The late SetCancel observes the request
// and cancels immediately; no delayed killer is lost or left untracked.
type killStage struct {
	mu              sync.Mutex
	cancel          context.CancelFunc
	cancelRequested bool
	done            chan struct{}
}

func newKillStage() *killStage {
	return &killStage{done: make(chan struct{})}
}

// Start atomically orders the stage's first side effect against cancellation.
// start runs with s.mu held because killTreeStaged sends its first signal
// synchronously. A Reset that wins RequestCancel prevents every signal; one
// that loses waits until the first signal has completed before proceeding.
func (s *killStage) Start(start func() context.CancelFunc) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelRequested {
		return false
	}
	s.cancel = start()
	return true
}

func (s *killStage) RequestCancel() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cancelRequested = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *killStage) CancelRequested() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelRequested
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
	mu      sync.RWMutex
	resetMu sync.Mutex // serializes Reset/ResetAndWait generation cuts
	jobs    map[string]*Job
	// draining owns jobs hidden by non-blocking Reset until cmd.Wait and every
	// staged killer have finished. ResetAndWait includes this set so an ordinary
	// session cleanup cannot make a later fullAccess revoke forget a process.
	draining   map[*Job]struct{}
	notify     chan Notification // buffered, drained by agent loop
	generation uint64            // incremented whenever session state is reset
	resetting  bool              // ResetAndWait admission fence

	// dir overrides ~/.metis/jobs (used by tests).
	dir string
}

// ErrRegistryResetting rejects new process ownership while ResetAndWait is
// joining the prior generation. Spawn checks this before Cmd.Start; Adopt
// leaves an already-running command with its caller when it returns this error.
var ErrRegistryResetting = errors.New("jobs registry is resetting")

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
		jobs:       make(map[string]*Job),
		draining:   make(map[*Job]struct{}),
		notify:     make(chan Notification, notifyBuf),
		generation: 1,
		dir:        filepath.Join(dir, "jobs"),
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
	// WaitResult transfers ownership of an already-started Cmd.Wait call.
	// The foreground Bash promotion path must race process completion
	// against its timer, so it starts the sole waiter before calling Adopt.
	// When non-nil, Registry consumes exactly one result from this channel
	// instead of calling Cmd.Wait a second time. Nil means Registry owns Wait.
	WaitResult <-chan error
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

	// Admission and publication bracket Cmd.Start under the same lock as
	// ResetAndWait's generation cut. Checking after Start would reject the map
	// insertion but still leak a newly-started process across the boundary.
	r.mu.Lock()
	if r.resetting {
		r.mu.Unlock()
		_ = out.Close()
		_ = os.Remove(outputPath)
		return nil, ErrRegistryResetting
	}
	if err := a.Cmd.Start(); err != nil {
		r.mu.Unlock()
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
		process:     a.Cmd.Process,
		cancel:      a.Cancel,
		output:      out,
		done:        make(chan struct{}),
		killStages:  make(map[*killStage]struct{}),
	}
	snapshot := publicJobSnapshot(j)

	j.generation = r.generation
	r.jobs[id] = j
	r.mu.Unlock()

	// Wait goroutine: blocks on Wait, transitions state, publishes
	// Notification. Outside the registry lock so JobOutput readers
	// can hit the disk file mid-run without serialising on r.mu.
	go r.waitAndComplete(j, nil)

	return snapshot, nil
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
		process:     a.Cmd.Process,
		cancel:      a.Cancel,
		output:      a.Output,
		done:        make(chan struct{}),
		killStages:  make(map[*killStage]struct{}),
	}

	r.mu.Lock()
	if r.resetting {
		r.mu.Unlock()
		return nil, ErrRegistryResetting
	}
	j.generation = r.generation
	r.jobs[id] = j
	snapshot := publicJobSnapshot(j)
	r.mu.Unlock()

	go r.waitAndComplete(j, a.WaitResult)

	return snapshot, nil
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

func (r *Registry) waitAndComplete(j *Job, waitResult <-chan error) {
	var err error
	if waitResult != nil {
		err = <-waitResult
	} else {
		err = j.cmd.Wait()
	}
	r.mu.Lock()
	if j.Status == StatusKilled {
		// JobStop already set the terminal state. Don't overwrite
		// (overwriting would race with the kill notifier).
		if j.output != nil {
			_ = j.output.Close()
		}
		j.cmd = nil
		j.output = nil
		j.leaderDone = true
		watchStart := r.registerTreeWatchLocked(j)
		r.maybeCloseLifecycleLocked(j)
		r.mu.Unlock()
		if watchStart != nil {
			r.startTrackedTreeWatch(*watchStart)
		}
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
	if j.output != nil {
		_ = j.output.Close()
	}
	j.cmd = nil
	j.output = nil
	j.leaderDone = true
	notif := Notification{
		JobID:    j.ID,
		Status:   j.Status,
		ExitCode: j.ExitCode,
		Elapsed:  j.EndTime.Sub(j.StartTime),
		Command:  security.RedactSubprocessText(j.Command),
	}
	generation := j.generation
	watchStart := r.registerTreeWatchLocked(j)
	r.maybeCloseLifecycleLocked(j)
	r.mu.Unlock()

	if watchStart != nil {
		r.startTrackedTreeWatch(*watchStart)
	}
	r.publish(generation, notif)
}

// maybeCloseLifecycleLocked linearizes the final ownership edge. A hidden job
// remains in draining until its leader is reaped and every staged killer (and
// its tracked context fallback) has returned.
func (r *Registry) maybeCloseLifecycleLocked(j *Job) {
	if j == nil || j.doneClosed || !j.leaderDone || len(j.killStages) != 0 {
		return
	}
	j.doneClosed = true
	j.process = nil
	j.cancel = nil
	delete(r.draining, j)
	close(j.done)
}

type stagedKillStart struct {
	job      *Job
	stage    *killStage
	process  *os.Process
	grace    time.Duration
	fallback context.CancelFunc
}

type stagedTreeWatchStart struct {
	job     *Job
	stage   *killStage
	process *os.Process
}

// registerTreeWatchLocked preserves the process-group identity when a leader
// exits naturally but same-group descendants remain. The watcher is passive:
// it sends no signal, and merely keeps the lifecycle edge open so a later
// Reset can atomically replace it with a terminating stage.
func (r *Registry) registerTreeWatchLocked(j *Job) *stagedTreeWatchStart {
	if j == nil || j.process == nil || len(j.killStages) != 0 || !isProcessTreeAlive(j.process) {
		return nil
	}
	stage := newKillStage()
	j.killStages[stage] = struct{}{}
	return &stagedTreeWatchStart{job: j, stage: stage, process: j.process}
}

func (r *Registry) finishTrackedStage(j *Job, stage *killStage) {
	r.mu.Lock()
	if _, ok := j.killStages[stage]; ok {
		delete(j.killStages, stage)
		close(stage.done)
	}
	r.maybeCloseLifecycleLocked(j)
	r.mu.Unlock()
}

// startTrackedKill starts a stage already registered in job.killStages. The
// stage's raw goroutine owns its timer; waiting for rawDone therefore also
// joins and releases that timer. Context cancellation is a tracked fallback,
// invoked only after a non-superseded tree-kill stage completes.
func (r *Registry) startTrackedKill(start stagedKillStart) {
	rawDone := make(chan struct{})
	if !start.stage.Start(func() context.CancelFunc {
		return killTreeStaged(start.process, start.grace, rawDone)
	}) {
		r.finishTrackedStage(start.job, start.stage)
		return
	}
	go func() {
		<-rawDone
		if !start.stage.CancelRequested() && start.fallback != nil {
			start.fallback()
		}
		r.finishTrackedStage(start.job, start.stage)
	}()
}

func (r *Registry) startTrackedTreeWatch(start stagedTreeWatchStart) {
	rawDone := make(chan struct{})
	if !start.stage.Start(func() context.CancelFunc {
		return watchProcessTree(start.process, rawDone)
	}) {
		r.finishTrackedStage(start.job, start.stage)
		return
	}
	go func() {
		<-rawDone
		r.finishTrackedStage(start.job, start.stage)
	}()
}

// publish delivers a terminal notification only while the job still belongs
// to the active top-level session. Holding the read lock through the
// non-blocking send makes Reset atomic with respect to publishers: a send that
// wins the race is drained by Reset, while one that loses observes the new
// generation and is discarded.
func (r *Registry) publish(generation uint64, notif Notification) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if generation != r.generation {
		return
	}
	// Non-blocking publish: if no one drains, drop. This keeps a runaway
	// "job spam" from deadlocking the wait goroutine. The model can still
	// see the completed status via JobList/JobOutput.
	select {
	case r.notify <- notif:
	default:
	}
}

// List returns detached value snapshots sorted by StartTime ascending.
// Callers may freely retain or mutate them: no entry aliases Registry's live
// state, so waitAndComplete can transition jobs concurrently without racing.
func (r *Registry) List() []Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, *publicJobSnapshot(j))
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

// Get returns a detached value snapshot of the Job with the given ID.
// The bool is false for an unknown ID. Internal process handles are omitted.
func (r *Registry) Get(id string) (Job, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	j, ok := r.jobs[id]
	if !ok || j == nil {
		return Job{}, false
	}
	return *publicJobSnapshot(j), true
}

// Snapshot returns a value copy of the Job's public, mutable fields
// (Status / EndTime / ExitCode + the immutable identifiers) taken
// under r.mu. The internal handles (cmd / cancel / output) are
// intentionally NOT copied — callers outside this package shouldn't
// touch them anyway.
//
// Retained as a descriptive alias for Get for callers that want to make the
// snapshot semantics explicit.
func (r *Registry) Snapshot(id string) (Job, bool) {
	return r.Get(id)
}

// publicJobSnapshot copies only externally observable state. The caller must
// hold r.mu once j is visible to waitAndComplete; Spawn and Adopt capture the
// initial snapshot before launching their waiter goroutine.
func publicJobSnapshot(j *Job) *Job {
	if j == nil {
		return nil
	}
	return &Job{
		ID:          j.ID,
		Command:     j.Command,
		Description: j.Description,
		Status:      j.Status,
		StartTime:   j.StartTime,
		EndTime:     j.EndTime,
		ExitCode:    j.ExitCode,
		OutputPath:  j.OutputPath,
	}
}

// CleanedUp reports whether the job's internal handles (cmd / cancel /
// output) have been released — i.e. waitAndComplete finished its
// cleanup branch. Read under r.mu to stay race-free against the spawn
// goroutine's nilling writes. Use this instead of `r.Get(id).cmd ==
// nil` from tests / external code.
func (r *Registry) CleanedUp(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	j, ok := r.jobs[id]
	if !ok || j == nil {
		return false
	}
	return j.cmd == nil && j.cancel == nil && j.output == nil
}

// ownsNaturallyCompletedTreeLocked identifies a terminal public Job whose
// leader has been reaped but whose passive tree watcher still owns same-PGID
// descendants. Status alone is not a lifecycle boundary for these jobs: Stop
// and Shutdown must be able to replace the watcher with a terminating stage.
// The caller must hold r.mu.
func ownsNaturallyCompletedTreeLocked(j *Job) bool {
	if j == nil || j.doneClosed || !j.leaderDone || j.process == nil || len(j.killStages) == 0 {
		return false
	}
	return j.Status == StatusCompleted || j.Status == StatusFailed
}

// Stop terminates a job using a two-stage tree-kill: SIGTERM the
// process group, wait `grace` for cooperative cleanup, then SIGKILL
// anything still alive. Returns an error only if the job ID is
// unknown — failed signal delivery on an already-exited process is fine and
// reported as success. A naturally terminal leader remains stoppable while a
// passive watcher still owns live same-group descendants.
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
	if j.Status != StatusRunning && !ownsNaturallyCompletedTreeLocked(j) {
		r.mu.Unlock()
		return nil // terminal with no retained process-tree ownership — no-op
	}
	process := j.process
	j.Status = StatusKilled
	j.EndTime = time.Now()
	j.ExitCode = -1
	command := j.Command
	elapsed := j.EndTime.Sub(j.StartTime)
	generation := j.generation
	var killStart *stagedKillStart
	if process != nil {
		// Two-stage tree kill. We deliberately don't wait for the
		// goroutine here — the caller (BashKill tool) wants to return
		// fast, and the wait goroutine watching cmd.Wait will observe
		// the SIGKILL exit and clean up its own state. Install ownership
		// while holding r.mu so waitAndComplete cannot miss a newly-created
		// delayed stage after cmd.Wait wins the race.
		stage := newKillStage()
		j.killStages[stage] = struct{}{}
		killStart = &stagedKillStart{
			job:      j,
			stage:    stage,
			process:  process,
			grace:    grace,
			fallback: j.cancel,
		}
	}
	r.mu.Unlock()

	if killStart != nil {
		r.startTrackedKill(*killStart)
	}

	r.publish(generation, Notification{
		JobID:    id,
		Status:   StatusKilled,
		ExitCode: -1,
		Elapsed:  elapsed,
		Command:  security.RedactSubprocessText(command),
	})
	return nil
}

// resetProcess carries one detached job's lifecycle edge plus the replacement
// stage registered by the generation cut. Superseded stages remain tracked by
// the Job until their goroutines exit; requesting cancellation never removes
// their ownership record early.
type resetProcess struct {
	job         *Job
	done        <-chan struct{}
	replacement *stagedKillStart
	superseded  []*killStage
}

// detachResetGeneration makes the old generation immediately invisible. It
// snapshots both public jobs and jobs hidden by an earlier non-blocking Reset,
// then installs every replacement stage in the same r.mu critical section.
// Thus cmd.Wait cannot close a lifecycle edge between old-stage cancellation
// and replacement publication.
func (r *Registry) detachResetGeneration(grace time.Duration, holdAdmission bool) []resetProcess {
	r.mu.Lock()
	if holdAdmission {
		r.resetting = true
	}
	r.generation++
	targets := make(map[*Job]struct{}, len(r.jobs)+len(r.draining))
	for _, j := range r.jobs {
		if j != nil {
			targets[j] = struct{}{}
		}
	}
	for j := range r.draining {
		if j != nil {
			targets[j] = struct{}{}
		}
	}
	running := make([]resetProcess, 0, len(targets))
	now := time.Now()
	for j := range targets {
		if j.doneClosed {
			continue
		}
		if j.Status == StatusRunning {
			j.Status = StatusKilled
			j.EndTime = now
			j.ExitCode = -1
		}
		r.draining[j] = struct{}{}
		p := resetProcess{job: j, done: j.done}
		for stage := range j.killStages {
			p.superseded = append(p.superseded, stage)
		}
		if j.process != nil {
			stage := newKillStage()
			j.killStages[stage] = struct{}{}
			p.replacement = &stagedKillStart{
				job:      j,
				stage:    stage,
				process:  j.process,
				grace:    grace,
				fallback: j.cancel,
			}
		}
		running = append(running, p)
	}
	r.jobs = make(map[string]*Job)

drainNotifications:
	for {
		select {
		case <-r.notify:
			continue
		default:
			break drainNotifications
		}
	}
	r.mu.Unlock()
	return running
}

func (r *Registry) startResetKills(running []resetProcess) {
	// Replacements are already registered under r.mu. Cancel old stages before
	// starting them so an old stage's first signal is either ordered before the
	// replacement or suppressed entirely; it can never resume afterward and hit
	// a reused PGID. Registry ownership remains continuous via the registered
	// replacement even during this hand-off.
	for i := range running {
		for _, stage := range running[i].superseded {
			stage.RequestCancel()
		}
	}
	for i := range running {
		if running[i].replacement != nil {
			r.startTrackedKill(*running[i].replacement)
		}
	}
}

// Reset terminates and forgets the current generation without waiting for
// process exit. This compatibility API remains appropriate for UI-oriented
// best-effort cleanup; security boundaries should use ResetAndWait.
func (r *Registry) Reset(grace time.Duration) {
	if r == nil {
		return
	}
	r.resetMu.Lock()
	running := r.detachResetGeneration(grace, false)
	r.startResetKills(running)
	r.resetMu.Unlock()
}

// ResetAndWait terminates and forgets the current generation, then waits for
// every Registry-owned cmd.Wait path and staged killer to finish. Any return is
// therefore a strong lifecycle boundary: no source-generation process remains
// executing or unreaped, and no delayed killer can target a reused PID/PGID.
func (r *Registry) ResetAndWait(grace time.Duration) {
	if r == nil {
		return
	}
	r.resetMu.Lock()
	defer r.resetMu.Unlock()
	running := r.detachResetGeneration(grace, true)
	defer func() {
		r.mu.Lock()
		r.resetting = false
		r.mu.Unlock()
	}()
	r.startResetKills(running)
	for _, p := range running {
		if p.done == nil {
			continue
		}
		<-p.done
	}
}

// Shutdown best-effort kills every running job and every naturally-terminal
// leader whose passive watcher still owns descendants. Called from agent.Loop
// teardown so a metis-quit doesn't orphan ~/.metis/jobs/<id>.out files or
// process trees.
func (r *Registry) Shutdown(grace time.Duration) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.jobs))
	for id, j := range r.jobs {
		if j.Status == StatusRunning || ownsNaturallyCompletedTreeLocked(j) {
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
