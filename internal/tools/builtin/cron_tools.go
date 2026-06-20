package builtin

// cron_tools.go — model-facing scheduling tools, claude-code's
// CronCreate / CronList / CronDelete (restored-src/src/tools/
// ScheduleCronTool/). They let the user schedule work conversationally
// ("every 5 min, check the build") instead of dropping to the `metis
// cron` CLI: the model translates the request into a CronJob.
//
// Plumbing mirrors ScheduleWakeup (wakeup.go) — each tool holds the
// session's *agent.CronService, wired by runtime/tools.go only when a
// service exists (the chat REPL), so headless/-p runs don't expose them.
//
// Firing model (claude-code parity):
//   - durable:false (DEFAULT) → an EPHEMERAL, session-only job: in-memory,
//     never written to disk, fired by the chat session's own in-session
//     scheduler while idle (SteerInject). Dies when this session ends.
//   - durable:true → a persisted job under ~/.metis/sessions/cron/, fired
//     by the standalone `metis cron start` daemon. Survives restarts;
//     because it's unattended it needs a pre-authorization allow-list
//     (see EvaluateCronPermission) or its write/exec calls get blocked.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ---- CronCreate ----------------------------------------------------------

type CronCreate struct {
	tools.BaseTool
	gate    *permission.Gate
	service *agent.CronService
}

func NewCronCreate(gate *permission.Gate, svc *agent.CronService) CronCreate {
	return CronCreate{gate: gate, service: svc}
}

func (CronCreate) Name() string { return "CronCreate" }

func (CronCreate) Description() string {
	return strings.TrimSpace(`
Schedule a prompt to run at a future time — recurring or one-shot. Use when
the user asks you to do something later or repeatedly ("every 5 minutes…",
"tomorrow at 9, …", "remind me in an hour").

Provide exactly ONE of:
  - every: a Go duration, e.g. "30s", "5m", "1h" (simple recurring interval)
  - cron:  a standard 5-field expression in LOCAL time "M H DoM Mon DoW"
           ("*/5 * * * *" = every 5 min, "0 9 * * 1-5" = weekdays 9am)
  - at:    an RFC3339 timestamp for a one-shot ("2026-06-20T09:00:00+08:00")

Tips:
  - Avoid the :00 and :30 minute marks when the time is approximate — every
    user asking for "9am" picks "0 9", spiking load. Nudge a few minutes:
    "around 9" → "0 9 * * *" becomes "57 8 * * *".
  - recurring (default true): fire every match until deleted. Set false for
    a one-shot that auto-deletes after firing once.

Durability:
  - durable=false (DEFAULT): session-only — runs in THIS chat while it's open
    and idle, then is gone. Right for "remind me in 10 min" / "check back in
    an hour". The user sees it run inline; permission prompts work normally.
  - durable=true: persists to disk and runs UNATTENDED via the "metis cron
    start" daemon, surviving restarts. Because no human is watching, list the
    tools it may use in 'allow' (e.g. ["Bash(git pull:*)","Write"]) or its
    write/exec/network calls are blocked. Only use durable when the user
    asks the task to persist across sessions.

Returns a job id for CronDelete.
`)
}

func (CronCreate) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"prompt"},
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "The prompt to run at each fire time. Make it self-contained — a fire starts a fresh turn that won't see the current conversation.",
			},
			"every": map[string]any{
				"type":        "string",
				"description": "Recurring interval as a Go duration: \"30s\", \"5m\", \"1h\". One of every/cron/at is required.",
			},
			"cron": map[string]any{
				"type":        "string",
				"description": "5-field cron expression in local time: \"M H DoM Mon DoW\".",
			},
			"at": map[string]any{
				"type":        "string",
				"description": "RFC3339 timestamp for a one-shot fire, e.g. \"2026-06-20T09:00:00+08:00\".",
			},
			"recurring": map[string]any{
				"type":        "boolean",
				"description": "true (default) = fire on every match until deleted. false = fire once then auto-delete (implied for `at`).",
			},
			"durable": map[string]any{
				"type":        "boolean",
				"description": "false (default) = session-only, fires in this chat while idle. true = persist to disk, fire unattended via `metis cron start`.",
			},
			"allow": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Pre-authorized tools for a DURABLE (unattended) job, in `Tool(content)` form, e.g. [\"Bash(git pull:*)\",\"Write\"]. Ignored for session-only jobs (those prompt the user live).",
			},
		},
	}
}

func (CronCreate) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (c CronCreate) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, _ := c.gate.Check(context.Background(), "CronCreate", strFromAny(in["prompt"]))
	return mapDecision(d), "schedule"
}

func (c CronCreate) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if c.service == nil {
		return errResult("CronCreate: cron service not available (only in chat sessions)"), nil
	}
	prompt := strings.TrimSpace(strFromAny(in["prompt"]))
	if prompt == "" {
		return errResult("CronCreate: prompt required"), nil
	}
	every := strings.TrimSpace(strFromAny(in["every"]))
	cronExpr := strings.TrimSpace(strFromAny(in["cron"]))
	at := strings.TrimSpace(strFromAny(in["at"]))
	if count := boolToInt(every != "") + boolToInt(cronExpr != "") + boolToInt(at != ""); count != 1 {
		return errResult("CronCreate: provide exactly one of every / cron / at"), nil
	}

	recurring := boolFromAny(in["recurring"], true)
	durable := boolFromAny(in["durable"], false)

	var sched agent.CronSchedule
	switch {
	case every != "":
		d, err := time.ParseDuration(every)
		if err != nil || d <= 0 {
			return errResult(fmt.Sprintf("CronCreate: invalid every %q (want a duration like 5m)", every)), nil
		}
		sched = agent.CronSchedule{Kind: "every", EveryMs: d.Milliseconds()}
	case cronExpr != "":
		sched = agent.CronSchedule{Kind: "cron", CronExpr: cronExpr}
	case at != "":
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			return errResult(fmt.Sprintf("CronCreate: invalid at %q (want RFC3339)", at)), nil
		}
		sched = agent.CronSchedule{Kind: "at", At: at}
		recurring = false // an explicit timestamp is inherently one-shot
	}

	job := &agent.CronJob{
		Name:      "chat: " + truncate(prompt, 40),
		Prompt:    prompt,
		Enabled:   true,
		Schedule:  sched,
		Ephemeral: !durable,
	}
	if !recurring {
		job.Repeat = 1 // fire once, then the service disables it
	}
	for _, raw := range anyToStrings(in["allow"]) {
		if r := strings.TrimSpace(raw); r != "" {
			job.AllowTools = append(job.AllowTools, r)
		}
	}

	if err := c.service.Create(job); err != nil {
		return errResult("CronCreate: " + err.Error()), nil
	}

	where := "session-only — fires in this chat while idle, gone when it ends"
	if durable {
		where = "durable — persisted; fires unattended via `metis cron start`"
		if len(job.AllowTools) == 0 {
			where += " (⚠ no tools pre-authorized: write/exec calls will be blocked — pass `allow`)"
		}
	}
	kind := "recurring"
	if !recurring {
		kind = "one-shot"
	}
	next := "—"
	if !job.NextRun.IsZero() {
		next = job.NextRun.Format(time.RFC3339)
	}
	return &tools.Result{
		Output: fmt.Sprintf("scheduled %s job %s (next: %s). %s. Cancel with CronDelete %s.",
			kind, job.ID, next, where, job.ID),
	}, nil
}

// ---- CronList ------------------------------------------------------------

type CronList struct {
	tools.BaseTool
	service *agent.CronService
}

func NewCronList(svc *agent.CronService) CronList { return CronList{service: svc} }

func (CronList) Name() string { return "CronList" }

func (CronList) Description() string {
	return "List scheduled cron jobs (both session-only and durable) created via CronCreate or the `metis cron` CLI."
}

func (CronList) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (CronList) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (CronList) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "" // read-only
}

func (c CronList) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	if c.service == nil {
		return errResult("CronList: cron service not available"), nil
	}
	jobs := c.service.List()
	if len(jobs) == 0 {
		return &tools.Result{Output: "No scheduled jobs."}, nil
	}
	var b strings.Builder
	for _, j := range jobs {
		scope := "durable"
		if j.Ephemeral {
			scope = "session-only"
		}
		state := "enabled"
		if j.Paused {
			state = "paused"
		} else if !j.Enabled {
			state = "disabled"
		}
		next := "—"
		if !j.NextRun.IsZero() {
			next = j.NextRun.Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "%s [%s, %s] next=%s: %s\n", j.ID, scope, state, next, truncate(j.Prompt, 60))
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// ---- CronDelete ----------------------------------------------------------

type CronDelete struct {
	tools.BaseTool
	gate    *permission.Gate
	service *agent.CronService
}

func NewCronDelete(gate *permission.Gate, svc *agent.CronService) CronDelete {
	return CronDelete{gate: gate, service: svc}
}

func (CronDelete) Name() string { return "CronDelete" }

func (CronDelete) Description() string {
	return "Cancel a scheduled cron job by its id (from CronCreate or CronList)."
}

func (CronDelete) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"id"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "Job id to cancel."},
		},
	}
}

func (CronDelete) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (c CronDelete) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, _ := c.gate.Check(context.Background(), "CronDelete", strFromAny(in["id"]))
	return mapDecision(d), "cancel scheduled job"
}

func (c CronDelete) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if c.service == nil {
		return errResult("CronDelete: cron service not available"), nil
	}
	id := strings.TrimSpace(strFromAny(in["id"]))
	if id == "" {
		return errResult("CronDelete: id required"), nil
	}
	if _, ok := c.service.Get(id); !ok {
		return errResult(fmt.Sprintf("CronDelete: no scheduled job with id %q", id)), nil
	}
	if err := c.service.Remove(id); err != nil {
		return errResult("CronDelete: " + err.Error()), nil
	}
	return &tools.Result{Output: "cancelled job " + id}, nil
}

// ---- small helpers -------------------------------------------------------

func errResult(msg string) *tools.Result { return &tools.Result{Output: msg, IsError: true} }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// boolFromAny coerces an untyped-JSON bool (bool, or "true"/"false" string
// some providers emit) with a default when absent/unparseable.
func boolFromAny(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "1":
			return true
		case "false", "no", "0":
			return false
		}
	}
	return def
}

// anyToStrings flattens a JSON array (or single string) into []string.
func anyToStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, strFromAny(e))
		}
		return out
	case string:
		if t != "" {
			return []string{t}
		}
	}
	return nil
}
