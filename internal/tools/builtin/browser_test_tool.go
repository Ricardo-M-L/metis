package builtin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// BrowserSuiteRunner is installed by the trusted host, never supplied by a
// model. It must return only after its finite test and cleanup have finished.
type BrowserSuiteRunner func(context.Context, string) (output string, passed bool, err error)

// BrowserTest exposes fixed suites, not a browser CDP endpoint, shell, URL or
// filesystem path. It is opt-in and is not registered by the ordinary runtime.
type BrowserTest struct {
	tools.BaseTool
	run  BrowserSuiteRunner
	gate *permission.Gate
}

func NewBrowserTest(run BrowserSuiteRunner, gate *permission.Gate) BrowserTest {
	return BrowserTest{run: run, gate: gate}
}
func (BrowserTest) Name() string      { return "BrowserTest" }
func (b BrowserTest) IsEnabled() bool { return b.run != nil }
func (BrowserTest) Description() string {
	return "Run a trusted fixed browser acceptance suite on the host-registered source snapshot. smoke builds, typechecks and opens real WebGL; controls tests driving, descent, pause and reset; endurance observes a real browser for 900 seconds. This call waits for completion and cleanup; no HTTP-only or virtual-time fallback. Results belong to the reported source hash, so rerun all required suites after source edits. No custom URL, shell, JavaScript or path is accepted."
}
func (BrowserTest) InputSchema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"suite"}, "additionalProperties": false,
		"properties": map[string]any{"suite": map[string]any{"type": "string", "enum": []string{"smoke", "controls", "endurance"}}}}
}
func (BrowserTest) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }
func (BrowserTest) TimeoutMs() int                               { return int((35 * time.Minute) / time.Millisecond) }
func (b BrowserTest) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	if b.gate != nil {
		suite, _ := in["suite"].(string)
		d, src := b.gate.Check(ctx, b.Name(), suite)
		return mapDecision(d), src
	}
	return tools.PermissionAllow, "host-configured fixed verification suite"
}
func (b BrowserTest) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	suite, _ := in["suite"].(string)
	if len(in) != 1 || (suite != "smoke" && suite != "controls" && suite != "endurance") {
		return &tools.Result{Output: "error: only suite=smoke|controls|endurance is accepted", IsError: true}, nil
	}
	if b.run == nil {
		return &tools.Result{Output: "environment_blocked: trusted browser runner is unavailable", IsError: true}, nil
	}
	// Covers bounded dependency preparation plus the 15-minute controls or
	// endurance suite and cleanup. This is a tool budget, not a run deadline.
	bounded, cancel := context.WithTimeout(ctx, 35*time.Minute)
	defer cancel()
	out, passed, err := b.run(bounded, suite)
	if err != nil {
		return nil, err
	}
	if err := bounded.Err(); err != nil {
		return nil, context.Cause(bounded)
	}
	return &tools.Result{Output: out, IsError: !passed}, nil
}

// TaskClock reports a live clock and monotonic elapsed time for this invocation,
// not the complete resumed benchmark. Startup prompt dates are not live clocks.
type TaskClock struct {
	tools.BaseTool
	started     time.Time
	runDeadline time.Time
}

func NewTaskClock(started, runDeadline time.Time) TaskClock {
	return TaskClock{started: started, runDeadline: runDeadline}
}
func (TaskClock) Name() string                   { return "TaskClock" }
func (TaskClock) IsReadOnly(map[string]any) bool { return true }
func (TaskClock) Description() string {
	return "Read actual current UTC time and this CLI invocation's monotonic elapsed seconds. A resumed invocation restarts this elapsed clock. The run deadline is null when the parent context has no deadline; use the benchmark's separately registered deadline, not startup metadata, for the total task budget."
}
func (TaskClock) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (TaskClock) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (TaskClock) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "read-only clock"
}
func (c TaskClock) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if len(in) != 0 {
		return &tools.Result{Output: "error: TaskClock accepts no arguments", IsError: true}, nil
	}
	now := time.Now()
	var deadline *string
	var remaining *float64
	if end := c.runDeadline; !end.IsZero() {
		value := end.UTC().Format(time.RFC3339Nano)
		deadline = &value
		seconds := max(0, time.Until(end).Seconds())
		remaining = &seconds
	}
	data, _ := json.Marshal(map[string]any{"current_time_utc": now.UTC().Format(time.RFC3339Nano), "invocation_elapsed_seconds": max(0, now.Sub(c.started).Seconds()), "run_deadline_utc": deadline, "run_remaining_seconds": remaining})
	return &tools.Result{Output: string(data)}, nil
}
