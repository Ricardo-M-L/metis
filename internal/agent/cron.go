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
	AllowTools    []string     `json:"allow_tools,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	LastRun       time.Time    `json:"last_run,omitempty"`
	NextRun       time.Time    `json:"next_run,omitempty"`
	RunCount      int          `json:"run_count"`
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
	running bool // true while a schedulerLoop goroutine owns `done`
}

// NewCronService creates a new cron service.
func NewCronService(root string) (*CronService, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &CronService{
		root: root,
		jobs: make(map[string]*CronJob),
		done: make(chan struct{}),
	}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *CronService) path(id string) string {
	return filepath.Join(s.root, id+".json")
}

func (s *CronService) loadAll() error {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-5]
		var job CronJob
		if err := s.loadJob(id, &job); err != nil {
			continue
		}
		s.jobs[id] = &job
	}
	return nil
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
	return os.WriteFile(s.path(job.ID), b, 0o644)
}

// Create adds a new cron job. Validates the schedule shape up-front so
// a bad cron expression is rejected at create time rather than silently
// disabling the job after the first scheduler tick.
func (s *CronService) Create(job *CronJob) error {
	if err := validateSchedule(job.Schedule); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	if err := validateSessionMode(job.SessionMode); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if job.ID == "" {
		job.ID = generateID()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	if job.Enabled {
		s.computeNextRun(job)
	}
	s.jobs[job.ID] = job
	// saveJob is a no-op for ephemeral (session-only) jobs — they stay in
	// memory, the in-session scheduler fires them, the daemon never learns
	// they exist.
	return s.saveJob(job)
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
	for _, j := range s.jobs {
		if !j.Ephemeral || !j.Enabled || j.Paused || j.NextRun.IsZero() {
			continue
		}
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			continue
		}
		if j.NextRun.After(now) {
			continue
		}
		j.LastRun = now
		j.RunCount++
		if j.Repeat > 0 && j.RunCount >= j.Repeat {
			j.Enabled = false
		}
		if j.Enabled && !j.Paused {
			s.computeNextRun(j)
		}
		fired = append(fired, j)
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
		out = append(out, j)
	}
	return out
}

// Get returns a job by ID.
func (s *CronService) Get(id string) (*CronJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

// Update modifies an existing job.
func (s *CronService) Update(id string, patch func(*CronJob)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job not found: %s", id)
	}
	patch(job)
	if job.Enabled && !job.Paused {
		s.computeNextRun(job)
	}
	return s.saveJob(job)
}

// Remove deletes a job.
func (s *CronService) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("job not found: %s", id)
	}
	delete(s.jobs, id)
	os.Remove(s.path(id))
	return nil
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
		if j.Enabled {
			s.computeNextRun(j)
		}
	})
}

// Run triggers immediate execution of a job.
func (s *CronService) Run(id string) error {
	job, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("job not found: %s", id)
	}
	return s.runJob(job)
}

// runJob executes a job's prompt.
func (s *CronService) runJob(job *CronJob) error {
	job.LastRun = time.Now()
	job.RunCount++
	if job.Repeat > 0 && job.RunCount >= job.Repeat {
		job.Enabled = false
	}
	if job.Enabled && !job.Paused {
		s.computeNextRun(job)
	}
	return s.saveJob(job)
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
	s.done = make(chan struct{})
	s.running = true
	s.mu.Unlock()

	go func() {
		s.schedulerLoop(ctx, onFire)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
}

func (s *CronService) schedulerLoop(ctx context.Context, onFire func(*CronJob) error) {
	for {
		s.reapExpired()
		s.mu.RLock()
		job, next := s.nextJob()
		s.mu.RUnlock()

		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case <-time.After(1 * time.Minute):
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-time.After(time.Until(next)):
			s.mu.Lock()
			if err := s.runJob(job); err == nil {
				onFire(job)
			}
			s.mu.Unlock()
		}
	}
}

// Stop halts the scheduler.
func (s *CronService) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// reapExpired drops jobs whose ExpiresAt has elapsed (from disk + memory).
// Called from schedulerLoop on each tick; takes the write lock itself so
// callers must NOT hold any lock when invoking.
func (s *CronService) reapExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, j := range s.jobs {
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			delete(s.jobs, id)
			_ = os.Remove(s.path(id))
		}
	}
}

// nextJob returns the job with the earliest NextRun time.
func (s *CronService) nextJob() (*CronJob, time.Time) {
	var earliest *CronJob
	var earliestTime time.Time
	now := time.Now()

	for _, j := range s.jobs {
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
			return j, now
		}
		if earliest == nil || j.NextRun.Before(earliestTime) {
			earliest = j
			earliestTime = j.NextRun
		}
	}
	if earliest != nil {
		return earliest, earliestTime
	}
	return nil, time.Now().Add(24 * time.Hour)
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
