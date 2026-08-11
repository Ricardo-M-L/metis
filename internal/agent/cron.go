package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser handles both 5-field standard expressions ("0 9 * * *") and
// 6-field "with seconds" expressions ("*/30 * * * * *"), plus descriptors
// like "@hourly" / "@daily" / "@every 1h30m". The earlier hand-rolled
// fallback (always advance by one hour) silently mis-scheduled every
// `kind=cron` job — the user could write any expression and metis would
// fire it hourly regardless.
var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// CronSchedule defines when a cron job should run.
//
// Kinds:
//   - "every" — fire repeatedly with EveryMs interval
//   - "cron"  — robfig/cron/v3 expression (5-field, 6-field with seconds,
//     or descriptors like "@daily" / "@every 1h30m"); evaluated
//     in the local timezone unless TZ is set
//   - "at"    — single ISO 8601 timestamp; advances by 24h after firing
//
// JitterMs (optional) adds [-jitter, +jitter] uniform noise to NextRun
// so multiple cron jobs scheduled for :00 don't all fire at the same
// instant (thundering-herd against rate-limited APIs). Recommended
// 30000-300000 (30s-5min) for non-time-critical jobs.
type CronSchedule struct {
	Kind     string `json:"kind"` // "every" | "cron" | "at"
	EveryMs  int64  `json:"every_ms,omitempty"`
	CronExpr string `json:"cron_expr,omitempty"`
	At       string `json:"at,omitempty"`
	JitterMs int64  `json:"jitter_ms,omitempty"`
	TZ       string `json:"tz,omitempty"` // IANA name (e.g. "America/Los_Angeles"); empty = local
}

// CronJob represents a scheduled job.
//
// SessionMode borrowed from openclaw's cron CLI — different jobs need
// different history strategies:
//
//   - "isolated" (default) — every fire starts with empty history.
//     Right for stateless reports ("post weekly metrics") that
//     shouldn't carry forward last week's analysis.
//   - "persistent" — one rolling thread per job, history accumulates
//     across fires. Right for an ongoing watchlist where the agent
//     should remember last-fire's findings to compare against.
//   - "main" — append to a named shared session (SessionRef, default
//     "main"). Right for "heartbeat"-style cron where multiple jobs
//     contribute to one continuous narrative.
//
// DisabledTools is a per-job runtime tool blacklist (Hermes pattern):
// while this job is firing, any tool name in the list is forced to
// PermissionDeny. Right for cron'd jobs where you want to prevent the
// agent from accidentally invoking expensive tools (Agent sub-spawn,
// WebFetch loops) without disabling them globally.
type CronJob struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Prompt        string       `json:"prompt"`
	Schedule      CronSchedule `json:"schedule"`
	Enabled       bool         `json:"enabled"`
	Paused        bool         `json:"paused"`
	Repeat        int          `json:"repeat,omitempty"` // 0 = infinite
	Skills        []string     `json:"skills,omitempty"`
	SessionMode   string       `json:"session_mode,omitempty"`
	SessionRef    string       `json:"session_ref,omitempty"`
	DisabledTools []string     `json:"disabled_tools,omitempty"`
	// AllowTools is the per-job pre-authorization allow-list — claude-code's
	// "always allow" rules adapted for UNATTENDED fires. A cron daemon has
	// no human to answer a mid-fire permission prompt, so the decision is
	// made entirely from this list, set ahead of time by the user (`cron
	// add --allow ...` / `cron allow <id> ...`). Each entry is a rule in
	// `Tool(content)` form: `Bash(git pull:*)`, `Write`, `Edit(/repo/**)`.
	// At fire time a tool call matching ANY entry runs without prompting;
	// anything else is denied and recorded to the denied store for the user
	// to review (`cron denied <id>`). Dangerous-pattern commands (rm -rf /,
	// fork bombs) stay denied even when allow-listed — pre-authorization
	// can't punch through the hard floor (see EvaluateCronPermission).
	AllowTools []string  `json:"allow_tools,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastRun    time.Time `json:"last_run,omitempty"`
	NextRun    time.Time `json:"next_run,omitempty"`
	RunCount   int       `json:"run_count"`
	// ExpiresAt — when set, the scheduler skips this job after the deadline
	// and the next List/persist sweep removes it. /loop sets this to 7 days
	// out by default so accidentally-left-running loops eventually clean
	// themselves up (claude-code's behavior).
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// Silent suppresses the per-fire chat / streaming surface and routes
	// the transcript to <root>/audit/<id>/<rfc3339>.jsonl instead. The
	// status bar still shows a "[cron N fired]" badge so the user knows
	// the job ran. Mirrors hermes-agent's SILENT_MARKER pattern: "I want
	// to know it happened, but don't interrupt me every time".
	//
	// Use for low-signal-per-fire jobs (every-5min health check, hourly
	// log scan) where 99% of fires have nothing interesting to surface
	// and the user will check audit logs when something looks off.
	// Loud (non-silent) is the default — silent is opt-in.
	Silent bool `json:"silent,omitempty"`

	// Ephemeral marks a session-only job — claude-code's `durable: false`
	// default. It lives only in this process's CronService (created via the
	// CronCreate tool during a chat) and is NEVER written to disk, so the
	// standalone `metis cron start` daemon (which reads jobs off disk in a
	// separate process) can't see it and there's no double-fire. The owning
	// chat session's in-session scheduler fires it via FireDueEphemeral
	// while idle and SteerInjects the prompt. Not serialized: anything on
	// disk is durable by definition, so this is always false after a load.
	Ephemeral bool `json:"-"`
}

// SessionMode constants. Use these instead of bare strings when
// constructing CronJob structs from Go callers.
const (
	SessionModeIsolated   = "isolated"
	SessionModePersistent = "persistent"
	SessionModeMain       = "main"
)

// CronService manages scheduled jobs.
type CronService struct {
	mu      sync.RWMutex
	root    string
	jobs    map[string]*CronJob
	done    chan struct{}
	stopped chan struct{}
	running bool // true while a schedulerLoop goroutine owns `done`

	// refreshInterval bounds how long a running daemon can retain a stale
	// view of durable jobs. CLI mutations happen in separate processes, so an
	// in-memory-only scheduler would otherwise never observe add/rm/pause/
	// resume/allow changes until restart. Tests shorten this interval.
	refreshInterval time.Duration
}

const defaultCronRefreshInterval = time.Second

// NewCronService creates a new cron service.
func NewCronService(root string) (*CronService, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &CronService{
		root:            root,
		jobs:            make(map[string]*CronJob),
		done:            make(chan struct{}),
		refreshInterval: defaultCronRefreshInterval,
	}
	if err := withCronStorageLock(root, s.loadAll); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *CronService) path(id string) string {
	return filepath.Join(s.root, id+".json")
}

func (s *CronService) readDurableJobs() (map[string]*CronJob, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	loaded := make(map[string]*CronJob)
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-5]
		var job CronJob
		if err := s.loadJob(id, &job); err != nil {
			continue
		}
		// The filename is the storage identity. Do not trust a stale or
		// hand-edited JSON id to redirect later saves to a different path.
		job.ID = id
		job.Ephemeral = false
		loaded[id] = &job
	}
	return loaded, nil
}

func (s *CronService) loadAll() error {
	loaded, err := s.readDurableJobs()
	if err != nil {
		return err
	}
	s.mergeDurableJobsLocked(loaded)
	return nil
}

// mergeDurableJobsLocked replaces only the disk-backed portion of the live
// map. The caller must hold s.mu when the service is already visible to other
// goroutines. Constructor-time loadAll is the only lock-free caller.
func (s *CronService) mergeDurableJobsLocked(loaded map[string]*CronJob) {
	for id, job := range s.jobs {
		if !job.Ephemeral {
			delete(s.jobs, id)
		}
	}
	for id, job := range loaded {
		// An ID collision is vanishingly unlikely, but a live session-only
		// job must never silently turn durable because a file appeared.
		if existing, ok := s.jobs[id]; ok && existing.Ephemeral {
			continue
		}
		s.jobs[id] = job
	}
}

// reloadDurableLocked refreshes the disk-backed portion of s.jobs. The caller
// must hold both s.mu for writing and the cross-process cron storage lock.
func (s *CronService) reloadDurableLocked() error {
	loaded, err := s.readDurableJobs()
	if err != nil {
		return err
	}
	s.mergeDurableJobsLocked(loaded)
	return nil
}

// reloadDurableFromDisk merges the latest cross-process durable state into
// the live service while preserving session-only jobs. Every `metis cron`
// invocation owns a separate CronService, so polling this storage boundary is
// what makes a long-running `cron start` daemon observe CRUD and allow-list
// changes made by later CLI/Desktop processes.
func (s *CronService) reloadDurableFromDisk() error {
	return withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.reloadDurableLocked()
	})
}

func (s *CronService) loadJob(id string, job *CronJob) error {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, job)
}

func (s *CronService) saveJob(job *CronJob) error {
	// Ephemeral (session-only) jobs must never touch disk — persisting one
	// would let the standalone daemon load + fire it, breaking the no-
	// double-fire invariant. Guarding the single persistence chokepoint
	// covers every path (Create / Update / Pause / Resume / runJob) so the
	// invariant can't be defeated by a future caller that forgets it.
	if job.Ephemeral {
		return nil
	}
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	// The daemon polls this directory while sibling CLI processes write it.
	// Publish via rename so a refresh can see either the old complete JSON or
	// the new complete JSON, never a truncate-in-progress document.
	tmp, err := os.CreateTemp(s.root, "."+job.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path(job.ID))
}

// cloneCronJob returns a fully independent public snapshot. CronJob contains
// slices, so a shallow struct copy would still let callers race with Update or
// the scheduler by mutating shared backing arrays after List/Get returns.
func cloneCronJob(job *CronJob) *CronJob {
	if job == nil {
		return nil
	}
	clone := *job
	clone.Skills = append([]string(nil), job.Skills...)
	clone.DisabledTools = append([]string(nil), job.DisabledTools...)
	clone.AllowTools = append([]string(nil), job.AllowTools...)
	return &clone
}

// Create adds a new cron job. Validates the schedule shape up-front so
// a bad cron expression is rejected at create time rather than silently
// disabling the job after the first scheduler tick.
func (s *CronService) Create(job *CronJob) error {
	if job == nil {
		return fmt.Errorf("cron job required")
	}
	if err := validateSchedule(job.Schedule); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	if err := validateSessionMode(job.SessionMode); err != nil {
		return err
	}
	created := cloneCronJob(job)
	if created.ID == "" {
		created.ID = generateID()
	}
	if created.CreatedAt.IsZero() {
		created.CreatedAt = time.Now()
	}
	if created.Enabled {
		s.computeNextRun(created)
	}
	commit := func() error {
		if !created.Ephemeral {
			// This service may have been constructed before a sibling CLI/Desktop
			// mutation. Refresh while holding the storage transaction so adding a
			// job does not leave its in-memory view stale.
			if err := s.reloadDurableLocked(); err != nil {
				return err
			}
		}
		// saveJob is a no-op for ephemeral (session-only) jobs — they stay in
		// memory, the in-session scheduler fires them, the daemon never learns
		// they exist.
		if err := s.saveJob(created); err != nil {
			return err
		}
		s.jobs[created.ID] = created
		return nil
	}
	var err error
	if created.Ephemeral {
		s.mu.Lock()
		defer s.mu.Unlock()
		err = commit()
	} else {
		err = withCronStorageLock(s.root, func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return commit()
		})
	}
	if err != nil {
		return err
	}
	// Preserve the long-standing caller contract: Create fills ID/CreatedAt/
	// NextRun on the supplied value, while the service keeps its own copy.
	*job = *cloneCronJob(created)
	return nil
}

// FireDueEphemeral advances and returns every ephemeral (session-only) job
// that is due at `now`, mirroring runJob's bookkeeping (LastRun, RunCount,
// Repeat→disable, computeNextRun) but WITHOUT saveJob — these jobs never
// hit disk. The TUI's in-session scheduler calls this on a tick and
// SteerInjects each returned job's prompt into the live session. Durable
// jobs are skipped entirely (they're the `metis cron start` daemon's job),
// so the two firing paths never overlap.
func (s *CronService) FireDueEphemeral(now time.Time) []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fired []*CronJob
	for id, j := range s.jobs {
		if !j.Ephemeral || !j.Enabled || j.Paused || j.NextRun.IsZero() {
			continue
		}
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			continue
		}
		if j.NextRun.After(now) {
			continue
		}
		advanced, err := s.advanceJobLocked(id, now)
		if err == nil {
			fired = append(fired, advanced)
		}
	}
	return fired
}

// validateSchedule checks the per-kind fields without mutating state.
// Returned errors are user-facing — they end up in `metis cron add`
// output, so they should name the offending field.
func validateSchedule(sc CronSchedule) error {
	switch sc.Kind {
	case "every":
		if sc.EveryMs <= 0 {
			return fmt.Errorf("every: every_ms must be > 0")
		}
	case "at":
		if sc.At == "" {
			return fmt.Errorf("at: at must be an RFC3339 timestamp")
		}
		if _, err := time.Parse(time.RFC3339, sc.At); err != nil {
			return fmt.Errorf("at: %w", err)
		}
	case "cron":
		if sc.CronExpr == "" {
			return fmt.Errorf("cron: cron_expr required")
		}
		if _, err := cronParser.Parse(sc.CronExpr); err != nil {
			return fmt.Errorf("cron: %w", err)
		}
	default:
		return fmt.Errorf("kind %q: must be one of every|cron|at", sc.Kind)
	}
	if sc.TZ != "" {
		if _, err := time.LoadLocation(sc.TZ); err != nil {
			return fmt.Errorf("tz: %w", err)
		}
	}
	return nil
}

// validateSessionMode rejects unknown session-mode values up-front.
// Empty is treated as "isolated" (legacy compatibility — existing on-
// disk jobs from before this field was introduced have no session_mode
// and should keep working).
func validateSessionMode(m string) error {
	switch m {
	case "", SessionModeIsolated, SessionModePersistent, SessionModeMain:
		return nil
	}
	return fmt.Errorf("session_mode %q: must be one of isolated|persistent|main", m)
}

// List returns all cron jobs.
func (s *CronService) List() []*CronJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*CronJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, cloneCronJob(j))
	}
	return out
}

// Get returns a job by ID.
func (s *CronService) Get(id string) (*CronJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return cloneCronJob(j), ok
}

// Update modifies an existing job.
func (s *CronService) Update(id string, patch func(*CronJob)) error {
	return withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		current, ok := s.jobs[id]
		if ok && current.Ephemeral {
			return s.updateJobLocked(id, current, patch)
		}
		var disk CronJob
		if err := s.loadJob(id, &disk); err != nil {
			if os.IsNotExist(err) {
				delete(s.jobs, id)
				return fmt.Errorf("job not found: %s", id)
			}
			return err
		}
		disk.ID = id
		disk.Ephemeral = false
		return s.updateJobLocked(id, &disk, patch)
	})
}

// updateJobLocked applies a patch to the supplied current snapshot. For a
// durable job, the caller must have loaded current while holding the cron
// storage lock so this read-modify-write cannot overwrite a sibling process.
func (s *CronService) updateJobLocked(id string, current *CronJob, patch func(*CronJob)) error {
	job := cloneCronJob(current)
	previousSchedule := job.Schedule
	wasEnabled := job.Enabled
	wasPaused := job.Paused
	patch(job)
	// Storage identity is controlled by the map key / filename, not by an
	// arbitrary patch callback.
	job.ID = id
	// Authorization and descriptive metadata updates must not postpone an
	// already-scheduled fire. Recompute only when the timing/lifecycle change
	// requires a new deadline (or a legacy job has none at all).
	scheduleChanged := job.Schedule != previousSchedule
	becameEnabled := !wasEnabled && job.Enabled
	resumed := wasPaused && !job.Paused
	if job.Enabled && !job.Paused && (scheduleChanged || becameEnabled || resumed || job.NextRun.IsZero()) {
		s.computeNextRun(job)
	}
	if err := s.saveJob(job); err != nil {
		return err
	}
	s.jobs[id] = job
	return nil
}

// Remove deletes a job.
func (s *CronService) Remove(id string) error {
	return withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if job, ok := s.jobs[id]; ok && job.Ephemeral {
			delete(s.jobs, id)
			return nil
		}
		var disk CronJob
		if err := s.loadJob(id, &disk); err != nil {
			if os.IsNotExist(err) {
				delete(s.jobs, id)
				return fmt.Errorf("job not found: %s", id)
			}
			return err
		}
		if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(s.jobs, id)
		return nil
	})
}

// Pause suspends a job.
func (s *CronService) Pause(id string) error {
	return s.Update(id, func(j *CronJob) {
		j.Paused = true
	})
}

// Resume continues a paused job.
func (s *CronService) Resume(id string) error {
	return s.Update(id, func(j *CronJob) {
		j.Paused = false
	})
}

// Run triggers immediate execution of a job.
func (s *CronService) Run(id string) error {
	_, err := s.RunNow(id)
	return err
}

// RunNow triggers immediate bookkeeping and returns the exact snapshot saved
// by that transaction. Callers that execute the prompt should use this rather
// than Run followed by Get, which would allow a sibling process to mutate or
// remove the job between those two operations.
func (s *CronService) RunNow(id string) (*CronJob, error) {
	var advanced *CronJob
	err := withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.jobs[id]; ok && current.Ephemeral {
			var err error
			advanced, err = s.advanceJobLocked(id, time.Now())
			return err
		}
		var disk CronJob
		if err := s.loadJob(id, &disk); err != nil {
			if os.IsNotExist(err) {
				delete(s.jobs, id)
				return fmt.Errorf("job not found: %s", id)
			}
			return err
		}
		disk.ID = id
		disk.Ephemeral = false
		s.jobs[id] = &disk
		var err error
		advanced, err = s.advanceJobLocked(id, time.Now())
		return err
	})
	return advanced, err
}

// advanceJobLocked records one firing and returns an immutable callback
// snapshot. The caller must hold s.mu for writing. Durable callers must also
// hold the cross-process cron storage lock and load the latest disk snapshot
// before calling.
func (s *CronService) advanceJobLocked(id string, ranAt time.Time) (*CronJob, error) {
	current, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	job := cloneCronJob(current)
	job.LastRun = ranAt
	job.RunCount++
	if job.Repeat > 0 && job.RunCount >= job.Repeat {
		job.Enabled = false
	}
	if job.Enabled && !job.Paused {
		s.computeNextRun(job)
	}
	if err := s.saveJob(job); err != nil {
		return nil, err
	}
	s.jobs[id] = job
	return cloneCronJob(job), nil
}

// ClearEphemeral drops every session-only job at a top-level chat-session
// boundary. Durable jobs remain process/global and continue to be managed by
// the standalone daemon.
func (s *CronService) ClearEphemeral() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, job := range s.jobs {
		if job.Ephemeral {
			delete(s.jobs, id)
			removed++
		}
	}
	return removed
}

// computeNextRun calculates the next run time based on schedule.
// Honors job.Schedule.TZ when set; falls back to time.Local otherwise.
// Adds Schedule.JitterMs of uniform noise on top of the deterministic
// next time so multiple :00-aligned jobs spread out across the minute.
func (s *CronService) computeNextRun(job *CronJob) {
	loc := time.Local
	if job.Schedule.TZ != "" {
		if l, err := time.LoadLocation(job.Schedule.TZ); err == nil {
			loc = l
		}
	}
	now := time.Now().In(loc)
	var next time.Time
	switch job.Schedule.Kind {
	case "every":
		if job.Schedule.EveryMs <= 0 {
			return
		}
		next = now.Add(time.Duration(job.Schedule.EveryMs) * time.Millisecond)
	case "at":
		t, err := time.ParseInLocation(time.RFC3339, job.Schedule.At, loc)
		if err != nil {
			return
		}
		if t.Before(now) {
			// "at" fired already — bump 24h so the same wall-clock
			// time tomorrow is the next firing. Callers who want a
			// one-shot should set Repeat=1 so RunCount disables it.
			next = t.Add(24 * time.Hour)
		} else {
			next = t
		}
	case "cron":
		sched, err := cronParser.Parse(job.Schedule.CronExpr)
		if err != nil {
			// Bad expression — disable the job rather than silently
			// firing on a wrong schedule. Caller can /cron list to
			// see Enabled=false and re-create with a valid expr.
			job.Enabled = false
			return
		}
		next = sched.Next(now)
	default:
		return
	}
	if job.Schedule.JitterMs > 0 {
		next = next.Add(jitter(job.Schedule.JitterMs))
	}
	job.NextRun = next
}

// jitter returns a uniformly-distributed offset in [-spanMs, +spanMs]
// using crypto/rand so multiple jobs created in the same millisecond
// (e.g. config-driven bulk add) actually disperse instead of all
// landing on the same shifted moment.
func jitter(spanMs int64) time.Duration {
	if spanMs <= 0 {
		return 0
	}
	max := big.NewInt(2*spanMs + 1)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64()-spanMs) * time.Millisecond
}

// Start begins the cron scheduler loop. It's a no-op if the loop is
// already running — calling Start twice without an intervening Stop used
// to spawn two scheduler goroutines that fired every job twice.
func (s *CronService) Start(ctx context.Context, onFire func(*CronJob) error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	s.done = done
	s.stopped = stopped
	s.running = true
	s.mu.Unlock()

	go func() {
		s.schedulerLoop(ctx, onFire, done)
		s.mu.Lock()
		if s.done == done {
			s.running = false
		}
		close(stopped)
		s.mu.Unlock()
	}()
}

func (s *CronService) schedulerLoop(ctx context.Context, onFire func(*CronJob) error, done <-chan struct{}) {
	refreshInterval := s.refreshInterval
	if refreshInterval <= 0 {
		refreshInterval = defaultCronRefreshInterval
	}
	for {
		// Durable CRUD is performed by short-lived sibling processes. Refresh
		// before choosing the next timer so the daemon never treats its startup
		// snapshot as authoritative forever.
		_ = s.refreshDurableState()
		s.mu.RLock()
		jobID, next := s.nextJob()
		s.mu.RUnlock()

		wait := refreshInterval
		if jobID != "" {
			until := time.Until(next)
			if until < 0 {
				until = 0
			}
			if until < wait {
				wait = until
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-done:
			timer.Stop()
			return
		case <-timer.C:
		}

		if jobID == "" {
			continue
		}

		// Re-read once more at the firing edge and advance within the same
		// cross-process transaction. A sibling pause/rm/allow may have landed
		// while this timer slept; the storage lock makes the eligibility check
		// and bookkeeping save one atomic read-modify-write operation.
		fired, _ := s.claimDueJob(jobID, time.Now())

		// A cron fire can run an LLM for minutes and may itself invoke cron
		// tools. Never retain the service mutex across that callback: CRUD must
		// remain responsive and recursive use must not deadlock.
		if fired != nil && onFire != nil {
			_ = onFire(fired)
		}
	}
}

// refreshDurableState reloads and reaps as one cross-process transaction.
// The file lock is always acquired before s.mu; every durable mutation uses
// this order so two CronService instances cannot deadlock each other.
func (s *CronService) refreshDurableState() error {
	return withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.reloadDurableLocked(); err != nil {
			return err
		}
		return s.reapExpiredLocked(time.Now())
	})
}

// claimDueJob atomically refreshes, revalidates, and advances a scheduled
// fire. It returns nil when a sibling process paused, removed, rescheduled, or
// already advanced the job before this scheduler acquired the storage lock.
func (s *CronService) claimDueJob(id string, now time.Time) (*CronJob, error) {
	var fired *CronJob
	err := withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.reloadDurableLocked(); err != nil {
			return err
		}
		if err := s.reapExpiredLocked(now); err != nil {
			return err
		}
		current, ok := s.jobs[id]
		if !ok || !current.Enabled || current.Paused || current.NextRun.IsZero() || current.NextRun.After(now) {
			return nil
		}
		var err error
		fired, err = s.advanceJobLocked(id, now)
		return err
	})
	return fired, err
}

// Stop halts the scheduler and waits for its goroutine to exit. Waiting keeps
// callers from tearing down the storage root while a final persistence pass is
// still in flight.
func (s *CronService) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	done := s.done
	stopped := s.stopped
	select {
	case <-done:
	default:
		close(done)
	}
	s.mu.Unlock()
	if stopped != nil {
		<-stopped
	}
}

// reapExpired drops jobs whose ExpiresAt has elapsed (from disk + memory).
// It reloads under the storage lock first so a stale service cannot delete a
// job whose deadline a sibling process just extended.
func (s *CronService) reapExpired() {
	_ = s.refreshDurableState()
}

// reapExpiredLocked removes expired jobs from memory and disk. The caller
// must hold the cross-process storage lock and s.mu for writing.
func (s *CronService) reapExpiredLocked(now time.Time) error {
	for id, j := range s.jobs {
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			if !j.Ephemeral {
				if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			delete(s.jobs, id)
		}
	}
	return nil
}

// nextJob returns the ID with the earliest NextRun time. The caller holds at
// least s.mu.RLock; returning an ID instead of the internal pointer prevents a
// stale pointer from escaping across the scheduler's sleep.
func (s *CronService) nextJob() (string, time.Time) {
	earliestID := ""
	var earliestTime time.Time
	now := time.Now()

	for id, j := range s.jobs {
		if !j.Enabled || j.Paused {
			continue
		}
		if j.NextRun.IsZero() {
			continue
		}
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			continue
		}
		if j.NextRun.Before(now) {
			return id, now
		}
		if earliestID == "" || j.NextRun.Before(earliestTime) {
			earliestID = id
			earliestTime = j.NextRun
		}
	}
	if earliestID != "" {
		return earliestID, earliestTime
	}
	return "", time.Now().Add(24 * time.Hour)
}

func generateID() string {
	return time.Now().UTC().Format("20060102150405") + "-" + randomString(8)
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail on modern OSes; fall back to a weak
		// timestamp-derived sequence to avoid panicking.
		seed := uint64(time.Now().UnixNano())
		for i := range buf {
			buf[i] = letters[seed%uint64(len(letters))]
			seed = seed*1103515245 + 12345
		}
		return string(buf)
	}
	for i := range buf {
		buf[i] = letters[int(buf[i])%len(letters)]
	}
	return string(buf)
}
