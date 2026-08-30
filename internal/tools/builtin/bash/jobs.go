package bash

// bash_jobs.go — three model-facing tools that pair with the Bash
// auto-background path: List / Output / Kill. Mirrors
// claude-code's TaskList / TaskOutput / TaskStop tooling, scoped to
// the bash job pool (we kept the agent-task tools — TaskOutput in
// task.go etc. — separate to avoid confusion with the per-session
// todo system).
//
// All three tools are registered when the runtime passes a
// jobs.Registry to BuildToolRegistry. AttachJobsRegistry binds the
// pool to the existing Bash entry so its 60s auto-background path
// works, AND registers the three reader tools. Without a pool,
// neither code path runs (Bash falls back to the foreground-only
// behavior).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// AttachJobsRegistry wires a jobs.Registry into the existing Bash
// entry in reg and registers List / Output / Kill. Idempotent
// — calling twice rebinds the registry on Bash and re-registers the
// three reader tools. Safe to call after builtin.Register.
//
// Why split this out from builtin.Register: jobs is owned by the
// runtime (lives one level up), and pulling it into builtin's
// register.go would create a runtime → builtin → jobs → ... cycle
// risk. Keeping AttachJobsRegistry as a post-step keeps the package
// graph clean.
func AttachJobsRegistry(reg *tools.Registry, pool *jobs.Registry, gate *permission.Gate) {
	if reg == nil || pool == nil {
		return
	}
	// Find the existing Bash registration and replace it with one
	// that has Jobs wired. Bash{} is a value type so we can't mutate
	// the registered instance — re-install via Registry.Replace which
	// overwrites by name (Register would panic on duplicate).
	if existing, ok := reg.Get("Bash"); ok {
		if b, ok := existing.(Bash); ok {
			b.Jobs = pool
			reg.Replace(b)
		}
	}
	reg.Replace(List{gate: gate, pool: pool})
	reg.Replace(Output{gate: gate, pool: pool})
	reg.Replace(Kill{gate: gate, pool: pool})
}

// ───────────────────────────────────────────────────────────────────
// List — "what background jobs are currently in flight?"
// ───────────────────────────────────────────────────────────────────

// List returns a JSON-serialized snapshot of the job pool. The
// model uses this to find a job_id to feed into Output / Kill,
// or to decide whether to wait vs spawn another. Empty input schema
// because there's nothing to filter on yet.
type List struct {
	tools.BaseTool
	gate *permission.Gate
	pool *jobs.Registry
}

func (List) Name() string { return "BashList" }
func (List) Description() string {
	return "List all background bash jobs (auto-promoted commands and explicit run_in_background). " +
		"Returns id, status, command preview, started time, elapsed, exit code (terminal only). " +
		"Empty list when no jobs are alive or have run this session."
}
func (List) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (List) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (l List) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := l.gate.Check(context.Background(), "List", "")
	return mapDecision(d), src
}
func (l List) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	if l.pool == nil {
		return &tools.Result{Output: "[]"}, nil
	}
	type row struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Command     string `json:"command"`
		Description string `json:"description"`
		StartTime   string `json:"start_time"`
		Elapsed     string `json:"elapsed"`
		ExitCode    *int   `json:"exit_code,omitempty"`
	}
	now := time.Now()
	jobsList := l.pool.List()
	out := make([]row, 0, len(jobsList))
	for _, j := range jobsList {
		end := j.EndTime
		if end.IsZero() {
			end = now
		}
		r := row{
			ID:          j.ID,
			Status:      j.Status.String(),
			Command:     security.RedactSubprocessText(j.Command),
			Description: security.RedactSubprocessText(j.Description),
			StartTime:   j.StartTime.Format(time.RFC3339),
			Elapsed:     end.Sub(j.StartTime).Truncate(time.Second).String(),
		}
		if j.Status != jobs.StatusRunning {
			ec := j.ExitCode
			r.ExitCode = &ec
		}
		out = append(out, r)
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: string(buf)}, nil
}

// ───────────────────────────────────────────────────────────────────
// Output — read a job's stdout/stderr capture from disk.
// ───────────────────────────────────────────────────────────────────

// Output returns the captured output of a background job. tail_max
// caps the number of bytes returned (default 50 KiB) — large logs are
// truncated from the head with a `[truncated head — file XX MiB]`
// marker so the model knows it's seeing the most recent activity.
//
// The job doesn't have to be done; reading mid-run is supported and
// returns whatever's been written so far.
type Output struct {
	tools.BaseTool
	gate *permission.Gate
	pool *jobs.Registry
}

func (Output) Name() string { return "BashOutput" }
func (Output) Description() string {
	return "Read the captured stdout/stderr of a background bash job. " +
		"Works on running and terminal jobs. " +
		"Use tail_max to cap returned bytes (default 50000); large logs are truncated from the head."
}
func (Output) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"job_id"},
		"properties": map[string]any{
			"job_id": map[string]any{
				"type":        "string",
				"description": "The job_id returned by Bash run_in_background or the auto-background promotion message",
			},
			"tail_max": map[string]any{
				"type":        "integer",
				"description": "Cap returned bytes; 0 = no cap; default 50000 (≈50 KiB).",
			},
		},
	}
}
func (Output) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (o Output) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := o.gate.Check(context.Background(), "Output", "")
	return mapDecision(d), src
}
func (o Output) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if o.pool == nil {
		return &tools.Result{Output: "[Output unavailable: jobs registry not wired]", IsError: true}, nil
	}
	id, _ := in["job_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("job_id is required")
	}
	j, ok := o.pool.Get(id)
	if !ok {
		return &tools.Result{
			Output:  fmt.Sprintf("[no such job %q — call List to see live jobs]", id),
			IsError: true,
		}, nil
	}
	tailMax := 50_000
	if v, ok := in["tail_max"].(float64); ok {
		tailMax = int(v)
	}
	body, err := jobs.ReadJobOutput(j.OutputPath, tailMax)
	if err != nil {
		return &tools.Result{
			Output:  fmt.Sprintf("[failed to read output for %s: %s]", id, security.RedactSubprocessText(err.Error())),
			IsError: true,
		}, nil
	}
	body = security.RedactSubprocessText(normalizeCapturedOutput(body))
	end := j.EndTime
	if end.IsZero() {
		end = time.Now()
	}
	header := fmt.Sprintf("[job %s · %s · started %s · elapsed %s",
		j.ID, j.Status, j.StartTime.Format(time.RFC3339),
		end.Sub(j.StartTime).Truncate(time.Second))
	if j.Status != jobs.StatusRunning {
		header += fmt.Sprintf(" · exit %d", j.ExitCode)
	}
	header += "]\n"
	return &tools.Result{Output: header + body}, nil
}

// ───────────────────────────────────────────────────────────────────
// Kill — terminate a background job.
// ───────────────────────────────────────────────────────────────────

// Kill sends SIGTERM (then SIGKILL after a 2s grace) to the
// process behind a job. Returns immediately; the job state will move
// to "killed" asynchronously. Calling on an already-terminal job is a
// silent no-op.
type Kill struct {
	tools.BaseTool
	gate *permission.Gate
	pool *jobs.Registry
}

func (Kill) Name() string { return "BashKill" }
func (Kill) Description() string {
	return "Stop a running background bash job. SIGTERM first, SIGKILL after 2s grace. " +
		"No-op on jobs that already exited."
}
func (Kill) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"job_id"},
		"properties": map[string]any{
			"job_id": map[string]any{"type": "string"},
		},
	}
}
func (Kill) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (k Kill) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	id, _ := in["job_id"].(string)
	d, src := k.gate.Check(ctx, "BashKill", id)
	return mapDecision(d), src
}
func (k Kill) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if k.pool == nil {
		return &tools.Result{Output: "[Kill unavailable: jobs registry not wired]", IsError: true}, nil
	}
	id, _ := in["job_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("job_id is required")
	}
	if err := k.pool.Stop(id, 2*time.Second); err != nil {
		// "unknown id" is a real error, "already terminal" comes back nil.
		if strings.Contains(err.Error(), "unknown id") {
			return &tools.Result{Output: err.Error(), IsError: true}, nil
		}
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("[kill request sent to %s — process will exit shortly]", id)}, nil
}
