package builtin

// wakeup.go — ScheduleWakeup tool. Lets the LLM self-pace a `/loop`-like
// continuation by saying "wake me in N seconds with this prompt".
//
// Borrowed straight from claude-code's bundled/loop.ts (`ScheduleWakeup`)
// — unlike a fixed-interval cron, this lets the model itself decide
// when the next attempt should happen. Two big wins:
//
//   1. Cache alignment — Anthropic prompt cache TTL is 5 minutes, so
//      delays under 270s keep the cache warm; jumping to 1200s+ amortizes
//      a single cache miss across a long quiet period. The tool's
//      schema text suggests these breakpoints in `delaySeconds`.
//
//   2. No blind polling — when an agent is mid-build it can wake every
//      60s; when it's idle it can sleep 30 minutes. A static cron can't.
//
// Implementation: a wakeup is just a one-shot cron job (kind="at",
// repeat=1). The cron service already handles firing + cleanup, so the
// only new code is this tool that constructs the right CronJob shape.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ScheduleWakeup is the LLM-facing tool. It needs a *agent.CronService
// reference so it can persist the wakeup; that pointer is wired in by
// Register() at startup, scoped to the same session as the agent loop.
type ScheduleWakeup struct {
	tools.BaseTool
	gate    *permission.Gate
	service *agent.CronService
}

// NewScheduleWakeup is the runtime-side constructor — keeps the
// service-injection plumbing out of the standard builtin.Register call
// (which doesn't have access to a CronService). Mirrors NewAgent +
// NewSendMessage's pattern in this package.
func NewScheduleWakeup(gate *permission.Gate, svc *agent.CronService) ScheduleWakeup {
	return ScheduleWakeup{gate: gate, service: svc}
}

func (ScheduleWakeup) Name() string { return "ScheduleWakeup" }

func (ScheduleWakeup) Description() string {
	return strings.TrimSpace(`
Schedule a future wake-up where you (the agent) re-enter with a prompt.
Use this to self-pace continuous work without blind polling.

Picking delaySeconds:
  - 60-270s: prompt cache (5min TTL) stays warm — right for active polling.
  - 300-1200s: pay one cache miss; right when nothing's about to change soon.
  - 1200s+: idle waiting; the user can interrupt sooner if they need you.

Provide a concise reason — it shows in telemetry and the user's status bar.
`)
}

func (ScheduleWakeup) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"delaySeconds", "prompt", "reason"},
		"properties": map[string]any{
			"delaySeconds": map[string]any{
				"type":        "integer",
				"description": "Seconds from now to wake up. Clamped to [30, 86400] (30s..24h).",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The prompt to feed back into the agent on wake. Should be self-contained (the new turn won't see this turn's context).",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "One short sentence on what you chose and why. Goes to telemetry and is shown to the user.",
			},
		},
	}
}

// Concurrency is Safe — the tool just writes a JSON file under
// ~/.metis/cron/. No long-running I/O, no shared mutable state aside
// from the CronService's internal mutex.
func (ScheduleWakeup) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (s ScheduleWakeup) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, _ := s.gate.Check(context.Background(), "ScheduleWakeup", strFromAny(in["reason"]))
	return mapDecision(d), "schedule"
}

func (s ScheduleWakeup) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if s.service == nil {
		return &tools.Result{
			Output:  "ScheduleWakeup: cron service not initialized",
			IsError: true,
		}, nil
	}
	delay, err := requireSeconds(in["delaySeconds"])
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	prompt := strings.TrimSpace(strFromAny(in["prompt"]))
	if prompt == "" {
		return &tools.Result{Output: "ScheduleWakeup: prompt required", IsError: true}, nil
	}
	reason := strings.TrimSpace(strFromAny(in["reason"]))
	if reason == "" {
		return &tools.Result{Output: "ScheduleWakeup: reason required", IsError: true}, nil
	}

	// Clamp range. 30s floor matches /cron's soft floor (sub-30s feels
	// like a hot-loop typo); 24h ceiling because anything longer should
	// be a real /cron with an explicit schedule the user can see.
	const minDelay, maxDelay = 30, 86400
	if delay < minDelay {
		delay = minDelay
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	// One-shot via Kind=at + Repeat=1. The cron service flips Enabled
	// to false after RunCount >= Repeat, so the wakeup self-cleans
	// without an explicit Remove call.
	at := time.Now().Add(time.Duration(delay) * time.Second)
	job := &agent.CronJob{
		Name:    "wakeup: " + truncate(reason, 40),
		Prompt:  prompt,
		Enabled: true,
		Repeat:  1,
		Schedule: agent.CronSchedule{
			Kind: "at",
			At:   at.Format(time.RFC3339),
		},
	}
	if err := s.service.Create(job); err != nil {
		return &tools.Result{
			Output:  "ScheduleWakeup: " + err.Error(),
			IsError: true,
		}, nil
	}
	return &tools.Result{
		Output: fmt.Sprintf("scheduled wakeup id=%s in %ds (%s) — %s",
			job.ID, delay, at.Format("15:04:05"), reason),
	}, nil
}

// requireSeconds normalizes a delaySeconds JSON value (int or float in
// untyped JSON) into a positive integer.
func requireSeconds(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		// Some providers stringify integers in tool inputs; accept it
		// rather than rejecting a request that's clearly intended.
		var out int
		if _, err := fmt.Sscanf(n, "%d", &out); err == nil {
			return out, nil
		}
	}
	return 0, fmt.Errorf("ScheduleWakeup: delaySeconds must be a positive integer")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
