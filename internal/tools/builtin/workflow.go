package builtin

// Workflow — run an ordered sequence of shell steps as one structured
// unit (build → test → lint, run-then-commit), with per-step status and
// stop-on-failure. See internal/workflow for the rationale vs a plain
// `cmd1 && cmd2` Bash chain (observability: the model sees exactly which
// step broke). Named workflows persist under ~/.metis/workflows so
// common sequences are reusable.
//
// Permission: every step command is gated through the SAME permission
// path as Bash (gate.Check with tool "Bash"), so a workflow can't run a
// command the user wouldn't have approved as a direct Bash call.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/shellguard"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

// Workflow is the LLM-facing tool.
type Workflow struct {
	tools.BaseTool
	gate       *permission.Gate
	store      *workflow.Store
	sandbox    *sandbox.Manager
	authorizer *invocationAuthorizer[workflowBinding]
}

type workflowBinding struct{ Name, VersionSHA256 string }

// NewWorkflow builds the tool. store may be nil (named-workflow
// persistence disabled); inline run still works.
func NewWorkflow(gate *permission.Gate, store *workflow.Store) *Workflow {
	return &Workflow{gate: gate, store: store, authorizer: newInvocationAuthorizer[workflowBinding]()}
}

// NewWorkflowWithSandbox builds Workflow with the runtime-owned sandbox
// Manager shared by Bash and Git.
func NewWorkflowWithSandbox(gate *permission.Gate, store *workflow.Store, manager *sandbox.Manager) *Workflow {
	return &Workflow{gate: gate, store: store, sandbox: manager, authorizer: newInvocationAuthorizer[workflowBinding]()}
}

// WithSandbox installs the runtime-owned Manager and returns w for fluent
// registry construction.
func (w *Workflow) WithSandbox(manager *sandbox.Manager) *Workflow {
	if w != nil {
		w.sandbox = manager
	}
	return w
}

// SandboxManager returns the Manager used for workflow subprocesses.
func (w *Workflow) SandboxManager() *sandbox.Manager {
	if w == nil {
		return nil
	}
	return w.sandbox
}

func (Workflow) Name() string { return "Workflow" }

func (Workflow) Description() string {
	return `Run an ordered sequence of shell steps as one unit, with per-step status. Prefer this over chaining commands with '&&' when you want to SEE which step failed: each step reports its own exit code and output, and a failure stops the run (later steps are marked skipped).

operation "run": execute inline steps now. Provide steps: [{name, command}, ...].
operation "save": propose a named workflow candidate. It becomes runnable after host evaluation and promotion with metis learning --kind workflow. Provide name + steps.
operation "run_named": run the active evaluated workflow version, or an existing host-saved baseline, by name.
operation "list": list saved workflow names.

Each step's command runs through the same permission checks as the Bash tool. Steps run in the current working directory. stop_on_error defaults to true.

Use for: build→test→lint gates, setup sequences, run-then-commit flows. For a single command, just use Bash.`
}

func (Workflow) InputSchema() map[string]any {
	stepSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":    map[string]any{"type": "string", "description": "Short label for this step, e.g. \"build\"."},
			"command": map[string]any{"type": "string", "description": "Shell command to run."},
		},
		"required": []string{"name", "command"},
	}
	return map[string]any{
		"type":     "object",
		"required": []string{"operation"},
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"enum":        []string{"run", "save", "run_named", "list"},
				"description": "run: execute inline steps. save: stage a named candidate for host evaluation. run_named: run the active version. list: list runnable names.",
			},
			"steps": map[string]any{
				"type":        "array",
				"items":       stepSchema,
				"description": "Ordered steps (for run / save).",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Workflow name. New candidates use 2..64 lowercase letters, digits, _ or -, starting with a letter/digit. Existing host workflow names are supported by run_named.",
			},
			"stop_on_error": map[string]any{
				"type":        "boolean",
				"description": "Stop after the first failing step, marking the rest skipped. Default true.",
			},
		},
	}
}

// Concurrency: Workflow runs shell commands, so it serializes like Bash
// write operations — Exclusive.
func (Workflow) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

// IsDestructive: a workflow runs arbitrary shell, so treat it as
// irreversible for the TUI's confirmation coloring.
func (Workflow) IsDestructive(map[string]any) bool { return true }

// CanUse gates every step command through the Bash permission path:
// strictest decision across the steps wins (any Deny → Deny, else any
// Ask → Ask, else Allow). "list" needs no command permission.
func (w Workflow) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	if err := w.checkLearningEvaluation(ctx, in); err != nil {
		return tools.PermissionDeny, err.Error()
	}
	op, _ := in["operation"].(string)
	if op == "list" {
		return tools.PermissionAllow, ""
	}
	cmds, wf, err := w.commandsFor(op, in)
	if err != nil {
		// Can't determine commands (e.g. unknown named workflow) — let
		// Execute surface the error; don't hard-deny on a lookup miss.
		return tools.PermissionAllow, ""
	}
	for _, command := range cmds {
		if w.fullAccess() {
			continue
		}
		if err := shellguard.Check(command); err != nil {
			return tools.PermissionDeny, err.Error()
		}
	}
	if w.gate == nil {
		if op == "run_named" {
			w.authorizer.record(ctx, workflowBinding{Name: wf.Name, VersionSHA256: wf.VersionSHA256})
		}
		return tools.PermissionAllow, ""
	}
	worst := tools.PermissionAllow
	var src string
	for _, c := range cmds {
		if strings.TrimSpace(c) == "" {
			continue
		}
		d, s := w.gate.Check(ctx, "Bash", c)
		switch mapDecision(d) {
		case tools.PermissionDeny:
			return tools.PermissionDeny, s
		case tools.PermissionAsk:
			if w.sandbox != nil && w.sandbox.AutoAllow() {
				continue
			}
			worst = tools.PermissionAsk
			src = s
		}
	}
	if op == "run_named" {
		w.authorizer.record(ctx, workflowBinding{Name: wf.Name, VersionSHA256: wf.VersionSHA256})
	}
	return worst, src
}

// Forked callers have already applied their immutable permission gate. Bind
// that authorized call to the current recipe without borrowing the parent gate.
func (w Workflow) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	op, _ := in["operation"].(string)
	if op != "run_named" {
		return nil
	}
	if err := w.checkLearningEvaluation(ctx, in); err != nil {
		return err
	}
	_, wf, err := w.commandsFor(op, in)
	if err != nil {
		return err
	}
	w.authorizer.record(ctx, workflowBinding{Name: wf.Name, VersionSHA256: wf.VersionSHA256})
	return nil
}

// commandsFor resolves the step commands a given operation will run, so
// CanUse and Execute share one source of truth.
func (w Workflow) commandsFor(op string, in map[string]any) (cmds []string, wf workflow.Workflow, err error) {
	switch op {
	case "run", "save":
		wf, err = parseInlineWorkflow(in)
	case "run_named":
		name, _ := in["name"].(string)
		if w.store == nil {
			return nil, workflow.Workflow{}, fmt.Errorf("named workflows are not available")
		}
		wf, err = w.store.Load(name)
	default:
		return nil, workflow.Workflow{}, fmt.Errorf("unknown operation %q", op)
	}
	if err != nil {
		return nil, workflow.Workflow{}, err
	}
	return wf.Commands(), wf, nil
}

func (w Workflow) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if err := w.checkLearningEvaluation(ctx, in); err != nil {
		return blockedWorkflowResult(err), nil
	}
	op, _ := in["operation"].(string)
	switch op {
	case "list":
		return w.execList()
	case "save":
		wf, err := parseInlineWorkflow(in)
		if err != nil {
			return &tools.Result{Output: "Workflow: " + err.Error(), IsError: true}, nil
		}
		if err := preflightWorkflow(wf, w.fullAccess()); err != nil {
			return blockedWorkflowResult(err), nil
		}
		return w.execSaveWorkflow(ctx, wf, in)
	case "run":
		wf, err := parseInlineWorkflow(in)
		if err != nil {
			return &tools.Result{Output: "Workflow: " + err.Error(), IsError: true}, nil
		}
		if err := preflightWorkflow(wf, w.fullAccess()); err != nil {
			return blockedWorkflowResult(err), nil
		}
		return w.execRun(ctx, wf, in), nil
	case "run_named":
		if w.store == nil {
			return &tools.Result{Output: "Workflow: named workflows are not available.", IsError: true}, nil
		}
		name, _ := in["name"].(string)
		wf, err := w.store.Load(name)
		if err != nil {
			return &tools.Result{Output: "Workflow: " + err.Error(), IsError: true}, nil
		}
		binding, dispatched, found := w.authorizer.consume(ctx)
		if dispatched && (!found || binding.Name != wf.Name || binding.VersionSHA256 != wf.VersionSHA256) {
			return blockedWorkflowResult(fmt.Errorf("named workflow changed or lacks its invocation permission binding")), nil
		}
		if err := preflightWorkflow(wf, w.fullAccess()); err != nil {
			return blockedWorkflowResult(err), nil
		}
		finish, err := learning.BeginInvocation(ctx, learning.Invocation{Kind: "workflow", ArtifactRoot: w.store.Dir(), Name: wf.Name, VersionSHA256: wf.VersionSHA256})
		if err != nil {
			return nil, err
		}
		result := w.execRun(ctx, wf, in)
		proof, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if err := finish(ctx, proof); err != nil {
			return nil, err
		}
		return result, nil
	default:
		return &tools.Result{Output: fmt.Sprintf("Workflow: unknown operation %q (want run/save/run_named/list).", op), IsError: true}, nil
	}
}

func preflightWorkflow(wf workflow.Workflow, skipDangerousCommandCheck bool) error {
	if skipDangerousCommandCheck {
		return nil
	}
	for _, step := range wf.Steps {
		if err := shellguard.Check(step.Command); err != nil {
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}
	return nil
}

func (w Workflow) fullAccess() bool {
	return w.gate != nil && w.gate.Mode() == permission.ModeFullAccess
}

func blockedWorkflowResult(err error) *tools.Result {
	return &tools.Result{Output: "Workflow: [blocked] " + err.Error(), IsError: true}
}

func (w Workflow) execRun(ctx context.Context, wf workflow.Workflow, in map[string]any) *tools.Result {
	if os.Getenv("METIS_LEARNING_WORKFLOW_NAME") != "" {
		if wf.VersionSHA256 != os.Getenv("METIS_LEARNING_WORKFLOW_VERSION") {
			return blockedWorkflowResult(fmt.Errorf("workflow evaluator recipe changed before execution"))
		}
		fixture, err := learningWorkflowFixture()
		if err != nil {
			return blockedWorkflowResult(err)
		}
		if err := workflow.ValidateEvaluationFixtureRecipe(wf, fixture); err != nil {
			return blockedWorkflowResult(err)
		}
	}
	stop := true
	if v, ok := in["stop_on_error"].(bool); ok {
		stop = v
	}
	cwd := agent.CwdFromContext(ctx)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	opts := workflow.RunOptions{
		Cwd:                       cwd,
		StopOnError:               stop,
		Sandbox:                   w.sandbox,
		SkipDangerousCommandCheck: w.fullAccess(),
	}
	if os.Getenv("METIS_LEARNING_WORKFLOW_NAME") != "" {
		opts.StopOnError = true
		opts.PerStepTimeout = 30 * time.Second
		opts.Network = sandbox.NetworkBlock
		opts.SkipDangerousCommandCheck = false
		opts.Environment = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + cwd}
		fixture, err := learningWorkflowFixture()
		if err != nil {
			return blockedWorkflowResult(err)
		}
		opts.EvaluationFixture = fixture
	}
	results := workflow.Run(ctx, wf, opts)
	if wf.VersionSHA256 != "" {
		body, _ := json.Marshal(workflow.RunRecord{Name: wf.Name, RecipeSHA256: wf.VersionSHA256, Steps: results})
		return &tools.Result{Output: string(body), IsError: workflow.Failed(results)}
	}

	var b strings.Builder
	label := wf.Name
	if label == "" {
		label = "(inline)"
	}
	fmt.Fprintf(&b, "Workflow %s — %d steps:\n\n", label, len(results))
	for _, r := range results {
		fmt.Fprintf(&b, "[%s] %s", r.Status, r.Name)
		if r.Status != workflow.StatusSkipped {
			fmt.Fprintf(&b, " (exit %d)", r.ExitCode)
		}
		b.WriteString("\n")
		if out := strings.TrimSpace(r.Output); out != "" {
			b.WriteString(indent(out, "    "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	res := &tools.Result{Output: strings.TrimRight(b.String(), "\n")}
	if workflow.Failed(results) {
		res.IsError = true
	}
	return res
}

func (w Workflow) execSave(in map[string]any) (*tools.Result, error) {
	if w.store == nil {
		return &tools.Result{Output: "Workflow: named workflows are not available.", IsError: true}, nil
	}
	wf, err := parseInlineWorkflow(in)
	if err != nil {
		return &tools.Result{Output: "Workflow: " + err.Error(), IsError: true}, nil
	}
	if err := preflightWorkflow(wf, w.fullAccess()); err != nil {
		return blockedWorkflowResult(err), nil
	}
	return w.execSaveWorkflow(context.Background(), wf, in)
}

func (w Workflow) execSaveWorkflow(ctx context.Context, wf workflow.Workflow, in map[string]any) (*tools.Result, error) {
	if w.store == nil {
		return &tools.Result{Output: "Workflow: named workflows are not available.", IsError: true}, nil
	}
	name, _ := in["name"].(string)
	wf.Name = name
	v, err := w.store.Propose(ctx, wf)
	if err != nil {
		return &tools.Result{Output: "Workflow: " + err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: fmt.Sprintf("Staged workflow %q candidate %s with %d steps. Active workflow unchanged. Host must evaluate and promote with metis learning --kind workflow before this candidate can run_named.", name, v.ID, len(wf.Steps))}, nil
}

// This boundary is installed only in the evaluator's private child process.
// Even direct Execute callers cannot substitute inline steps or a different
// saved workflow for the frozen candidate whose evidence the host will grade.
func (w Workflow) checkLearningEvaluation(ctx context.Context, in map[string]any) error {
	name := os.Getenv("METIS_LEARNING_WORKFLOW_NAME")
	if name == "" {
		return nil
	}
	version := os.Getenv("METIS_LEARNING_WORKFLOW_VERSION")
	op, _ := in["operation"].(string)
	target, _ := in["name"].(string)
	if op != "run_named" || target != name || len(version) != 64 || w.store == nil {
		return fmt.Errorf("workflow evaluator permits only the pinned run_named recipe")
	}
	if w.sandbox == nil || w.sandbox.EffectiveMode() == sandbox.ModeOff || w.sandbox.NetworkPolicy() != sandbox.NetworkBlock || w.fullAccess() {
		return fmt.Errorf("workflow evaluator requires an enabled network-blocking sandbox")
	}
	cwd := agent.CwdFromContext(ctx)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd != os.Getenv("METIS_LEARNING_WORKFLOW_CWD") {
		return fmt.Errorf("workflow evaluator working directory changed")
	}
	wf, err := w.store.Load(name)
	if err != nil {
		return err
	}
	if wf.VersionSHA256 != version {
		return fmt.Errorf("workflow evaluator recipe version changed")
	}
	fixture, err := learningWorkflowFixture()
	if err != nil {
		return err
	}
	return workflow.ValidateEvaluationFixtureRecipe(wf, fixture)
}

func learningWorkflowFixture() (*workflow.EvaluationFixtureContext, error) {
	path := os.Getenv("METIS_LEARNING_FIXTURE_CONTEXT")
	if path == "" {
		return nil, nil
	}
	return workflow.LoadEvaluationFixtureContext(path, os.Getenv("METIS_LEARNING_FIXTURE_CONTEXT_SHA256"), os.Getenv("METIS_LEARNING_WORKFLOW_CWD"))
}

func (w Workflow) execList() (*tools.Result, error) {
	if w.store == nil {
		return &tools.Result{Output: "Workflow: named workflows are not available."}, nil
	}
	names, err := w.store.List()
	if err != nil {
		return &tools.Result{Output: "Workflow: " + err.Error(), IsError: true}, nil
	}
	if len(names) == 0 {
		return &tools.Result{Output: "No saved workflows yet."}, nil
	}
	body, _ := json.Marshal(map[string]any{"workflows": names})
	return &tools.Result{Output: string(body)}, nil
}

// parseInlineWorkflow extracts steps[] from the tool input.
func parseInlineWorkflow(in map[string]any) (workflow.Workflow, error) {
	raw, ok := in["steps"].([]any)
	if !ok || len(raw) == 0 {
		return workflow.Workflow{}, fmt.Errorf("`steps` is required and must be a non-empty array")
	}
	var wf workflow.Workflow
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return workflow.Workflow{}, fmt.Errorf("step %d is not an object", i)
		}
		name, _ := m["name"].(string)
		cmd, _ := m["command"].(string)
		if strings.TrimSpace(cmd) == "" {
			return workflow.Workflow{}, fmt.Errorf("step %d (%q) has an empty command", i, name)
		}
		if name == "" {
			name = fmt.Sprintf("step-%d", i+1)
		}
		wf.Steps = append(wf.Steps, workflow.Step{Name: name, Command: cmd})
	}
	return wf, nil
}

// indent prefixes every line of s with prefix.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
