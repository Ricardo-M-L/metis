package builtin

// ScheduleWakeup persists a one-shot original-session continuation. Its live
// consumer uses claim/start/completion receipts; ordinary cron daemons must not
// execute it. Cache retention remains provider-dependent.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ScheduleWakeup is the LLM-facing tool. It needs a *agent.CronService
// reference so it can persist the wakeup; that pointer is wired in by
// Register() at startup, scoped to the same session as the agent loop.
type ScheduleWakeup struct {
	tools.BaseTool
	gate     *permission.Gate
	service  *agent.CronService
	identity agent.ContinuationIdentity
}

// NewScheduleWakeup is the runtime-side constructor — keeps the
// service-injection plumbing out of the standard builtin.Register call
// (which doesn't have access to a CronService). Mirrors NewAgent +
// NewSendMessage's pattern in this package.
func NewScheduleWakeup(gate *permission.Gate, svc *agent.CronService) ScheduleWakeup {
	return ScheduleWakeup{gate: gate, service: svc}
}

func (s ScheduleWakeup) WithContinuation(identity agent.ContinuationIdentity) ScheduleWakeup {
	s.identity = identity
	return s
}

func (ScheduleWakeup) Name() string { return "ScheduleWakeup" }

func (ScheduleWakeup) Description() string {
	return strings.TrimSpace(`
Schedule a future continuation in this original conversation, using its existing
context and permissions. This is not an unattended cron task and grants no tool
permissions. If the session is closed, the wakeup remains pending until that
same session is resumed; no independent daemon is started.

Choose a delay appropriate to the work (30 seconds to 24 hours). Cache retention
is provider-dependent and is not guaranteed by scheduling a wakeup.
Provide a concise reason — it is shown to the user.
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
				"description": "The prompt to continue in this original session on wake. Existing conversation context is preserved.",
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

func (s ScheduleWakeup) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	// A turn-pinned session must agree with registry binding. Never redirect an
	// old registry to whichever Desktop tab happens to be globally current.
	if id := tasks.SessionIDFromContext(ctx); id != "" && s.identity.SessionID != "" && id != s.identity.SessionID {
		return &tools.Result{Output: "ScheduleWakeup: original session binding mismatch", IsError: true}, nil
	}
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

	// Persist pending work. Admission disables it atomically; Start counts actual
	// execution and Finish records a terminal outcome without deleting history.
	at := time.Now().Add(time.Duration(delay) * time.Second)
	job := &agent.CronJob{
		Kind:               agent.CronKindContinuation,
		OriginSessionID:    s.identity.SessionID,
		WorkDir:            s.identity.WorkDir,
		Provider:           s.identity.Provider,
		Model:              s.identity.Model,
		ContinuationStatus: "pending",
		SessionMode:        agent.SessionModeMain,
		SessionRef:         s.identity.SessionID,
		Name:               "wakeup: " + truncate(reason, 40),
		Prompt:             prompt,
		Enabled:            true,
		Repeat:             1,
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
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}
