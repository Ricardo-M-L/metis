package builtin

// monitor.go — Monitor tool. Spawns a background command (same path as
// `Bash --run_in_background=true`) AND attaches a per-line pattern
// watcher; matches push <monitor_event> system-reminders into the next
// model turn so the agent reacts without having to poll bash.Output.
//
// Pattern parity:
//   - claude-code's Monitor tool — single-script "tell me each time
//     the filter emits". User-facing semantics are identical.
//   - hermes-agent process_registry.py:_check_watch_patterns — same
//     rate-limit-then-mute behavior (3 strikes within 15s → silent).
//   - openclaude MonitorTool — same `description` + watch shape, just
//     written for TS/Ink instead of Go/lipgloss.
//
// metis's twist: we re-use the existing jobs.Registry (Spawn +
// DiskOutput + Notify) and the per-line scan + emit lives in the agent
// monitor.go layer. The tool itself just orchestrates compile patterns
// → Spawn → Watch.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/shellguard"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

// Monitor is the tool struct. Wires through Permission for gating
// (same as Bash — a Monitor command CAN do anything bash can; the gate
// pre-checks the command string against the user's allow/deny rules)
// and the agent.MonitorRegistry that the loop drains.
type Monitor struct {
	tools.BaseTool
	Jobs     *jobs.Registry
	Watches  *agent.MonitorRegistry
	gate     *permission.Gate
	settings config.ToolBashSettings
	sandbox  *sandbox.Manager
}

// NewMonitor wires the tool with the registries the agent loop owns.
// Returning nil when either registry is missing is intentional — a
// caller without backing infra (sub-agent, headless test) shouldn't
// see the tool listed at all.
func NewMonitor(j *jobs.Registry, w *agent.MonitorRegistry, gate *permission.Gate, settings config.ToolBashSettings) *Monitor {
	return NewMonitorWithSandbox(j, w, gate, settings, nil)
}

// NewMonitorWithSandbox constructs Monitor with the runtime-owned sandbox
// manager shared by the other model-controlled command tools.
func NewMonitorWithSandbox(j *jobs.Registry, w *agent.MonitorRegistry, gate *permission.Gate, settings config.ToolBashSettings, manager *sandbox.Manager) *Monitor {
	if j == nil || w == nil {
		return nil
	}
	return &Monitor{Jobs: j, Watches: w, gate: gate, settings: settings, sandbox: manager}
}

// WithSandbox installs the runtime-owned sandbox manager.
func (m *Monitor) WithSandbox(manager *sandbox.Manager) *Monitor {
	if m != nil {
		m.sandbox = manager
	}
	return m
}

// SandboxManager exposes the injected manager for runtime wiring tests.
func (m Monitor) SandboxManager() *sandbox.Manager { return m.sandbox }

func (m Monitor) wrapCommand(cmd *exec.Cmd) (*exec.Cmd, error) {
	if m.sandbox == nil {
		mode, err := sandbox.ParseMode(m.settings.Sandbox.Mode)
		if err != nil {
			return nil, err
		}
		if mode != sandbox.ModeOff {
			return nil, fmt.Errorf("sandbox mode %q requires a runtime sandbox manager", mode)
		}
		return cmd, nil
	}
	network := sandbox.NetworkAllow
	if !m.settings.Sandbox.DangerouslyAllowNetwork && strings.EqualFold(m.settings.Sandbox.Network, "block") {
		network = sandbox.NetworkBlock
	}
	return m.sandbox.Wrap(cmd, sandbox.Request{Cwd: cmd.Dir, Network: network})
}

// AttachMonitorRegistry registers the Monitor tool. Called from the
// runtime layer alongside AttachJobsRegistry so a single registry-
// build pass wires both the bg-bash readers AND the per-line watcher.
// Idempotent: a second call replaces the existing Monitor binding.
//
// Splitting from AttachJobsRegistry keeps the older function's
// signature stable (no agent.MonitorRegistry param) — callers that
// don't need watching can wire jobs without touching monitor.
func AttachMonitorRegistry(reg *tools.Registry, j *jobs.Registry, w *agent.MonitorRegistry, gate *permission.Gate, settings config.ToolBashSettings) {
	AttachMonitorRegistryWithSandbox(reg, j, w, gate, settings, nil)
}

// AttachMonitorRegistryWithSandbox registers Monitor with the same sandbox
// manager used by Bash and Workflow.
func AttachMonitorRegistryWithSandbox(reg *tools.Registry, j *jobs.Registry, w *agent.MonitorRegistry, gate *permission.Gate, settings config.ToolBashSettings, manager *sandbox.Manager) {
	tool := NewMonitorWithSandbox(j, w, gate, settings, manager)
	if tool == nil {
		return
	}
	reg.Replace(*tool)
}

func (Monitor) Name() string { return "Monitor" }

func (Monitor) Description() string {
	return "Spawn a background process whose output is regex-watched line-by-line. " +
		"Each matching line becomes a <monitor_event> system-reminder before your next turn. " +
		"Use for: tail-grep loops, until-condition polls, file-change watchers. " +
		"DON'T use to run a script you just want completed (use Bash run_in_background=true instead)."
}

func (Monitor) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"command", "description"},
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to run in the background. Stdout+stderr are tailed.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Short label for the watcher (e.g. 'dev server ERROR lines'). Shown in <monitor_event>.",
			},
			"watch_patterns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Regex patterns (RE2 syntax). A line matching ANY pattern fires an event. Default: every non-empty line matches.",
			},
		},
	}
}

func (Monitor) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

// CanUse defers to permission gating like Bash — "Monitor sleep 5"
// shouldn't bypass the user's deny-rules just because it's a Monitor
// instead of a Bash. The gate sees the literal command string.
func (m Monitor) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	cmd, _ := in["command"].(string)
	if cmd == "" {
		return tools.PermissionDeny, "Monitor: empty command"
	}
	if err := shellguard.Check(cmd); err != nil {
		return tools.PermissionDeny, "Monitor: " + err.Error()
	}
	if m.gate != nil {
		decision, src := m.gate.Check(ctx, "Bash", cmd)
		switch decision {
		case permission.DecisionAllow:
			return tools.PermissionAllow, src
		case permission.DecisionDeny:
			return tools.PermissionDeny, src
		}
	}
	return tools.PermissionAsk, "Monitor spawns a background bash process — confirm before running"
}

// Execute compiles patterns, spawns the command, registers the watch.
// Returns immediately with the job id — the model gets <monitor_event>
// reminders as matches fire (drained at iteration boundaries).
func (m Monitor) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	cmd, _ := in["command"].(string)
	desc, _ := in["description"].(string)
	if cmd == "" {
		return &tools.Result{Output: "Monitor: command required", IsError: true}, nil
	}
	if err := shellguard.Check(cmd); err != nil {
		return &tools.Result{Output: "Monitor: " + err.Error(), IsError: true}, nil
	}
	if desc == "" {
		desc = "(no description)"
	}

	// Compile patterns up-front so syntax errors come back as a tool
	// error (the model can fix and retry) rather than silently
	// disabling the watch.
	patterns, perr := compilePatterns(in["watch_patterns"])
	if perr != "" {
		return &tools.Result{Output: "Monitor: " + perr, IsError: true}, nil
	}
	// Empty patterns slice → match-everything sentinel. The model
	// asked for an unfiltered tail; fire on every non-empty line.
	if len(patterns) == 0 {
		patterns = []*regexp.Regexp{regexp.MustCompile(`.+`)}
	}

	// Background context — Monitor outlives the foreground turn just
	// like Bash run_in_background. Reusing bash.go's executeBackground
	// would be ideal but it's tightly coupled to Bash struct; copying
	// the spawn shape here keeps the new tool self-contained.
	bgCtx, cancel := context.WithCancel(context.Background())
	_ = ctx
	shell := m.settings.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	exe := jobs.OOMWrappedCommand(bgCtx, shell, cmd)
	if m.sandbox != nil {
		exe.Env = m.sandbox.FilterEnv(os.Environ(), m.settings.Sandbox.DangerouslyInheritEnv)
	} else {
		exe.Env = bash.FilterEnv(os.Environ(), m.settings.Sandbox.DangerouslyInheritEnv)
	}
	exe.Env = bash.ApplyNetworkPolicy(exe.Env, m.settings.Sandbox)
	if cwd := agent.CwdFromContext(ctx); cwd != "" {
		exe.Dir = cwd
	}
	wrapped, err := m.wrapCommand(exe)
	if err != nil {
		cancel()
		return &tools.Result{Output: "Monitor: sandbox wrap failed: " + err.Error(), IsError: true}, nil
	}
	exe = wrapped
	jobs.ApplyProcessGroup(exe)

	jb, err := m.Jobs.Spawn(jobs.SpawnArgs{
		Command: cmd,
		Cmd:     exe,
		Cancel:  cancel,
	})
	if err != nil {
		cancel()
		return &tools.Result{Output: fmt.Sprintf("Monitor: spawn failed: %v", err), IsError: true}, nil
	}

	// jb.OutputPath is the file the watch will tail.
	m.Watches.Watch(jb.ID, jb.OutputPath, desc, patterns)

	return &tools.Result{
		Output: fmt.Sprintf(
			"[monitor active, job_id=%s, %d pattern(s)] watching: %s\n"+
				"Each matching line becomes a <monitor_event> on your next turn.\n"+
				"Use bash.Output {job_id: %q} to read context, bash.Kill {job_id: %q} to stop.",
			jb.ID, len(patterns), desc, jb.ID, jb.ID,
		),
	}, nil
}

// compilePatterns walks the JSON-decoded watch_patterns slice and
// compiles each into RE2. Returns a non-empty error string for any
// pattern that fails to compile so the model gets actionable feedback.
func compilePatterns(raw any) ([]*regexp.Regexp, string) {
	if raw == nil {
		return nil, ""
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, "watch_patterns must be an array of strings"
	}
	var patterns []*regexp.Regexp
	for i, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Sprintf("watch_patterns[%d] is not a string", i)
		}
		if s == "" {
			continue
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Sprintf("watch_patterns[%d] (%q): %v", i, s, err)
		}
		patterns = append(patterns, re)
	}
	return patterns, ""
}
