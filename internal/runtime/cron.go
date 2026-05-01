package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
)

// cronRoot returns the on-disk directory the cron service uses for jobs.
// Centralized so chat handlers and the CLI can both find the same place
// without re-deriving the path.
func cronRoot(cfg *config.Config) string {
	return filepath.Join(cfg.Session.Dir, "cron")
}

// CronChatService is a thin wrapper around agent.CronService scoped to the
// rendering needs of the /cron slash command. We construct it per-invocation
// (cheap — file-backed, lazy load) so handlers don't have to thread a long-
// lived dependency through main.go's runtime struct just for chat use.
type CronChatService struct {
	svc *agent.CronService
}

// NewCronChatService reads the on-disk job set so the caller can immediately
// List/Add/etc. without further ceremony.
func NewCronChatService(cfg *config.Config) (*CronChatService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cron: nil config")
	}
	svc, err := agent.NewCronService(cronRoot(cfg))
	if err != nil {
		return nil, err
	}
	return &CronChatService{svc: svc}, nil
}

// AllJobs returns the underlying CronService's full job list. Exposed so
// callers (e.g. /loop) can filter by their own naming convention without
// going through the rendered output.
func (s *CronChatService) AllJobs() []*agent.CronJob {
	return s.svc.List()
}

// RenderList formats every cron job as a one-liner. Empty registry returns a
// friendly hint instead of an empty string so the chat surface always has
// something to show.
func (s *CronChatService) RenderList() string {
	jobs := s.svc.List()
	if len(jobs) == 0 {
		return "(no cron jobs — `/cron add <every> <prompt>` to create one)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d cron job(s):\n", len(jobs))
	for _, j := range jobs {
		state := "enabled"
		if !j.Enabled {
			state = "disabled"
		} else if j.Paused {
			state = "paused"
		}
		next := "—"
		if !j.NextRun.IsZero() {
			next = j.NextRun.Format("2006-01-02 15:04")
		}
		// Trim long prompts so a chatty job doesn't push the layout.
		prompt := j.Prompt
		if len(prompt) > 60 {
			prompt = prompt[:57] + "…"
		}
		fmt.Fprintf(&b, "  %s  %-8s  next=%s  %q\n", j.ID, state, next, prompt)
	}
	return strings.TrimRight(b.String(), "\n")
}

// AddLoop is Add + an explicit ExpiresAt deadline. /loop sets a 7-day TTL
// so accidentally-left-running loops self-cleanup (claude-code parity).
// expiresAt zero ⇒ no TTL.
func (s *CronChatService) AddLoop(every, prompt string, expiresAt time.Time) (string, error) {
	id, err := s.Add(every, prompt)
	if err != nil {
		return "", err
	}
	if !expiresAt.IsZero() {
		if err := s.svc.Update(id, func(j *agent.CronJob) { j.ExpiresAt = expiresAt }); err != nil {
			return id, err
		}
	}
	return id, nil
}

// Add creates a new repeating cron job. `every` is parsed as time.Duration
// (e.g. "5m", "1h", "30s"); `prompt` is the user prompt the agent should
// run on each fire. Empty inputs error so a typo can't write a blank job.
func (s *CronChatService) Add(every, prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	every = strings.TrimSpace(every)
	if every == "" {
		return "", fmt.Errorf("cron: --every duration required")
	}
	if prompt == "" {
		return "", fmt.Errorf("cron: prompt required")
	}
	d, err := time.ParseDuration(every)
	if err != nil {
		return "", fmt.Errorf("cron: invalid interval %q: %w", every, err)
	}
	if d < 30*time.Second {
		// Soft floor — sub-30s schedules suggest a typo more often than a
		// genuine need; the CronService still tolerates them, but we nudge
		// the user away from accidentally creating a hot loop.
		return "", fmt.Errorf("cron: interval too short (min 30s); %s would hammer the scheduler", every)
	}
	job := &agent.CronJob{
		Name:    truncatePrompt(prompt, 40),
		Prompt:  prompt,
		Enabled: true,
		Schedule: agent.CronSchedule{
			Kind:    "every",
			EveryMs: d.Milliseconds(),
		},
	}
	if err := s.svc.Create(job); err != nil {
		return "", err
	}
	return job.ID, nil
}

// Remove deletes a job by id. Returns a clear error when the id is missing
// so the chat surface can echo it back rather than the user seeing a raw
// "file not found".
func (s *CronChatService) Remove(id string) error {
	if id == "" {
		return fmt.Errorf("cron: id required")
	}
	if _, ok := s.svc.Get(id); !ok {
		return fmt.Errorf("cron: no job with id %q", id)
	}
	return s.svc.Remove(id)
}

// Pause / Resume toggle a job's active state without removing it.
func (s *CronChatService) Pause(id string) error {
	if id == "" {
		return fmt.Errorf("cron: id required")
	}
	return s.svc.Pause(id)
}

func (s *CronChatService) Resume(id string) error {
	if id == "" {
		return fmt.Errorf("cron: id required")
	}
	return s.svc.Resume(id)
}

func truncatePrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
