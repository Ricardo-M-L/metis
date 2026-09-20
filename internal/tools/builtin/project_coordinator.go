package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ProjectCoordinator is the LLM-facing surface for the durable project graph.
// It complements the session-scoped Task* tools: use Task* for a temporary
// chat checklist, and this tool when work must survive session switches,
// separate workers, or a Desktop restart.
type ProjectCoordinator struct {
	tools.BaseTool
	gate              *permission.Gate
	store             *projectcoord.Store
	defaultWorkspace  string
	workspaceResolver func() string
}

// NewProjectCoordinator constructs the tool with an already configured,
// private Store. defaultWorkspace is captured at runtime bootstrap, while a
// sub-agent's context cwd takes precedence for a worktree/direct child.
func NewProjectCoordinator(gate *permission.Gate, store *projectcoord.Store, defaultWorkspace string) ProjectCoordinator {
	return ProjectCoordinator{
		gate:             gate,
		store:            store,
		defaultWorkspace: defaultWorkspace,
	}
}

// WithWorkspaceResolver keeps the default workspace aligned with a Desktop
// session switch.  It is evaluated at tool-call time; sub-agent Cwd context
// and an explicit input.workspace still take priority.
func (p ProjectCoordinator) WithWorkspaceResolver(resolver func() string) ProjectCoordinator {
	p.workspaceResolver = resolver
	return p
}

func (ProjectCoordinator) Name() string { return "ProjectCoordinator" }
func (ProjectCoordinator) Description() string {
	return "Manage a durable project work graph across METIS sessions and workers. Create a project run, inspect its dependency-aware status, add focused work, claim one ready item for a worker, then record completion or a concrete failure/recovery. The graph retains execution-environment rules and stale-worker recovery."
}
func (ProjectCoordinator) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "list", "status", "add_work", "claim_next", "complete", "fail", "recover"},
				"description": "create a run, list/status it, add a graph node, claim ready work, or record a lifecycle transition",
			},
			"workspace": map[string]any{"type": "string", "description": "absolute project directory; omit to use the active workspace"},
			"run_id":    map[string]any{"type": "string", "description": "durable project run ID"},
			"goal":      map[string]any{"type": "string", "description": "project objective for action=create"},
			"item_id":   map[string]any{"type": "string", "description": "work-item ID for complete/fail/recover; optional stable ID for add_work"},
			"phase": map[string]any{
				"type": "string", "enum": []string{"research", "synthesis", "implementation", "verification", "custom"},
				"description": "phase for add_work (default custom)",
			},
			"subject":      map[string]any{"type": "string", "description": "short work title for add_work"},
			"prompt":       map[string]any{"type": "string", "description": "focused worker instruction for add_work"},
			"depends_on":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "work-item IDs that must complete first"},
			"max_attempts": map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "description": "attempt budget for add_work; default 2"},
			"worker":       map[string]any{"type": "string", "description": "worker identity for claim/complete/fail"},
			"lease_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400,
				"description": "claim duration; expired claims are automatically requeued"},
			"output":           map[string]any{"type": "string", "description": "durable worker result/evidence for complete or fail"},
			"failure_code":     map[string]any{"type": "string", "description": "stable failure code for fail"},
			"failure_summary":  map[string]any{"type": "string", "description": "concrete reason for fail"},
			"suggested_action": map[string]any{"type": "string", "description": "how the coordinator can recover a failure"},
			"note":             map[string]any{"type": "string", "description": "why a failed item is safe to recover"},
			"force":            map[string]any{"type": "boolean", "description": "allow the one explicit retry after the normal attempt budget"},
		},
	}
}

func (ProjectCoordinator) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (p ProjectCoordinator) CanUse(ctx context.Context, _ map[string]any) (tools.Permission, string) {
	d, source := p.gate.Check(ctx, p.Name(), "")
	return mapDecision(d), source
}

func (p ProjectCoordinator) Execute(ctx context.Context, input map[string]any) (*tools.Result, error) {
	if p.store == nil {
		return nil, fmt.Errorf("project coordinator storage is unavailable")
	}
	action, _ := input["action"].(string)
	action = strings.TrimSpace(action)
	workspace, err := p.workspace(ctx, input)
	if err != nil {
		return nil, err
	}
	switch action {
	case "create":
		goal, _ := input["goal"].(string)
		run, err := p.store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: goal})
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"run": compactProjectRun(run)})
	case "list":
		runs, err := p.store.List(workspace)
		if err != nil {
			return nil, err
		}
		views := make([]projectRunView, 0, len(runs))
		for _, run := range runs {
			views = append(views, compactProjectRun(run))
		}
		return projectCoordinatorResult(map[string]any{"workspace": workspace, "runs": views})
	case "status":
		runID, err := requiredProjectString(input, "run_id")
		if err != nil {
			return nil, err
		}
		run, err := p.store.Get(workspace, runID)
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"run": compactProjectRun(run)})
	case "add_work":
		runID, err := requiredProjectString(input, "run_id")
		if err != nil {
			return nil, err
		}
		item, err := p.store.AddWorkItem(workspace, runID, projectcoord.WorkItemInput{
			ID:          stringProjectValue(input, "item_id"),
			Phase:       projectcoord.Phase(defaultProjectValue(input, "phase", string(projectcoord.PhaseCustom))),
			Subject:     stringProjectValue(input, "subject"),
			Prompt:      stringProjectValue(input, "prompt"),
			DependsOn:   projectStringSlice(input["depends_on"]),
			MaxAttempts: projectInt(input, "max_attempts", 0),
		})
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"item": compactProjectItem(item)})
	case "claim_next":
		runID, err := requiredProjectString(input, "run_id")
		if err != nil {
			return nil, err
		}
		worker, err := requiredProjectString(input, "worker")
		if err != nil {
			return nil, err
		}
		item, err := p.store.ClaimNext(workspace, runID, projectcoord.ClaimOptions{
			Worker: worker,
			Lease:  time.Duration(projectInt(input, "lease_seconds", 15*60)) * time.Second,
		})
		if err != nil {
			return nil, err
		}
		run, err := p.store.Get(workspace, runID)
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"item": compactProjectItem(item), "worker_prompt": projectcoord.BuildWorkerPrompt(run, item)})
	case "complete":
		runID, itemID, err := projectRunAndItem(input)
		if err != nil {
			return nil, err
		}
		item, err := p.store.Complete(workspace, runID, itemID, stringProjectValue(input, "worker"), stringProjectValue(input, "output"))
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"item": compactProjectItem(item)})
	case "fail":
		runID, itemID, err := projectRunAndItem(input)
		if err != nil {
			return nil, err
		}
		item, err := p.store.Fail(workspace, runID, itemID, stringProjectValue(input, "worker"), projectcoord.Failure{
			Code:            defaultProjectValue(input, "failure_code", projectcoord.FailureWorkerExecution),
			Summary:         stringProjectValue(input, "failure_summary"),
			SuggestedAction: stringProjectValue(input, "suggested_action"),
		}, stringProjectValue(input, "output"))
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"item": compactProjectItem(item)})
	case "recover":
		runID, itemID, err := projectRunAndItem(input)
		if err != nil {
			return nil, err
		}
		force, _ := input["force"].(bool)
		item, err := p.store.Recover(workspace, runID, itemID, stringProjectValue(input, "note"), force)
		if err != nil {
			return nil, err
		}
		return projectCoordinatorResult(map[string]any{"item": compactProjectItem(item)})
	default:
		return nil, fmt.Errorf("unsupported ProjectCoordinator action %q", action)
	}
}

func (p ProjectCoordinator) workspace(ctx context.Context, input map[string]any) (string, error) {
	value := stringProjectValue(input, "workspace")
	if value == "" {
		value = agent.CwdFromContext(ctx)
	}
	if value == "" {
		if p.workspaceResolver != nil {
			value = strings.TrimSpace(p.workspaceResolver())
		}
	}
	if value == "" {
		value = p.defaultWorkspace
	}
	if value == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		value = cwd
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func projectCoordinatorResult(value any) (*tools.Result, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: string(data)}, nil
}

func requiredProjectString(input map[string]any, key string) (string, error) {
	value := stringProjectValue(input, key)
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func projectRunAndItem(input map[string]any) (string, string, error) {
	runID, err := requiredProjectString(input, "run_id")
	if err != nil {
		return "", "", err
	}
	itemID, err := requiredProjectString(input, "item_id")
	if err != nil {
		return "", "", err
	}
	return runID, itemID, nil
}

func stringProjectValue(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func defaultProjectValue(input map[string]any, key, fallback string) string {
	if value := stringProjectValue(input, key); value != "" {
		return value
	}
	return fallback
}

func projectInt(input map[string]any, key string, fallback int) int {
	switch value := input[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

func projectStringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return strings
		}
		return nil
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		if item, ok := raw.(string); ok && strings.TrimSpace(item) != "" {
			result = append(result, strings.TrimSpace(item))
		}
	}
	return result
}

// projectRunView prevents an old worker's full output from taking over a new
// coordinator context.  Full evidence remains durable in the state file and
// the direct prerequisite excerpt is supplied only when claim_next needs it.
type projectRunView struct {
	ID          string                 `json:"id"`
	Workspace   string                 `json:"workspace"`
	Goal        string                 `json:"goal"`
	Status      projectcoord.RunStatus `json:"status"`
	Environment projectEnvironmentView `json:"environment"`
	Items       []projectWorkItemView  `json:"items"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

type projectEnvironmentView struct {
	GitRepository  bool   `json:"git_repository"`
	LinkedWorktree bool   `json:"linked_worktree"`
	RememberedRule string `json:"remembered_rule,omitempty"`
}

type projectWorkItemView struct {
	ID          string                  `json:"id"`
	Phase       projectcoord.Phase      `json:"phase"`
	Subject     string                  `json:"subject"`
	Status      projectcoord.ItemStatus `json:"status"`
	DependsOn   []string                `json:"depends_on,omitempty"`
	Owner       string                  `json:"owner,omitempty"`
	Attempts    int                     `json:"attempts"`
	MaxAttempts int                     `json:"max_attempts"`
	Output      string                  `json:"output_excerpt,omitempty"`
	Failure     *projectcoord.Failure   `json:"failure,omitempty"`
}

func compactProjectRun(run projectcoord.Run) projectRunView {
	view := projectRunView{
		ID:        run.ID,
		Workspace: run.Workspace,
		Goal:      run.Goal,
		Status:    run.Status,
		Environment: projectEnvironmentView{
			GitRepository:  run.Environment.IsGitRepository,
			LinkedWorktree: run.Environment.IsLinkedWorktree,
		},
		UpdatedAt: run.UpdatedAt,
		Items:     make([]projectWorkItemView, 0, len(run.Items)),
	}
	if run.Environment.LastFailure != nil {
		view.Environment.RememberedRule = run.Environment.LastFailure.Summary
	}
	for _, item := range run.Items {
		view.Items = append(view.Items, compactProjectItem(item))
	}
	return view
}

func compactProjectItem(item projectcoord.WorkItem) projectWorkItemView {
	view := projectWorkItemView{
		ID:          item.ID,
		Phase:       item.Phase,
		Subject:     item.Subject,
		Status:      item.Status,
		DependsOn:   append([]string(nil), item.DependsOn...),
		Owner:       item.Owner,
		Attempts:    item.Attempts,
		MaxAttempts: item.MaxAttempts,
		Output:      projectExcerpt(item.Output, 6000),
	}
	if item.Failure != nil {
		failure := *item.Failure
		view.Failure = &failure
	}
	return view
}

func projectExcerpt(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n… output excerpt truncated …"
}
