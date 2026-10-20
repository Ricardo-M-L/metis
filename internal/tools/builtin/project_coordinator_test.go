package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/execution"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func TestProjectCoordinatorCreatesClaimsAndCompletesDurableWork(t *testing.T) {
	workspace := t.TempDir()
	tool := NewProjectCoordinator(
		permission.New(permission.ModeBypass),
		projectcoord.NewStore(filepath.Join(t.TempDir(), "projects")),
		workspace,
	)
	created, err := tool.Execute(context.Background(), map[string]any{"action": "create", "goal": "Make recovery durable"})
	if err != nil {
		t.Fatal(err)
	}
	var createPayload struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(created.Output), &createPayload); err != nil {
		t.Fatal(err)
	}
	if createPayload.Run.ID == "" {
		t.Fatalf("create output = %s", created.Output)
	}

	claimed, err := tool.Execute(context.Background(), map[string]any{
		"action": "claim_next", "run_id": createPayload.Run.ID, "worker": "researcher",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(claimed.Output, `"id": "research"`) || !strings.Contains(claimed.Output, "Git repository: no") {
		t.Fatalf("claim output = %s", claimed.Output)
	}
	var claimPayload struct {
		Item struct {
			Lease projectcoord.Lease `json:"lease"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(claimed.Output), &claimPayload); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"action": "complete", "run_id": createPayload.Run.ID, "item_id": "research", "worker": "researcher", "lease_token": claimPayload.Item.Lease.Token, "output": "repository inspected",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"action": "complete", "run_id": createPayload.Run.ID, "item_id": "synthesis", "worker": "researcher", "lease_token": claimPayload.Item.Lease.Token, "output": "stale result",
	}); err == nil {
		t.Fatal("tool completed an unclaimed item with an old token")
	}
	status, err := tool.Execute(context.Background(), map[string]any{"action": "status", "run_id": createPayload.Run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Output, `"id": "synthesis"`) || !strings.Contains(status.Output, `"status": "ready"`) {
		t.Fatalf("status output = %s", status.Output)
	}
}

func TestProjectCoordinatorReadOnlyClassification(t *testing.T) {
	tool := ProjectCoordinator{}
	if !tool.IsReadOnly(map[string]any{"action": "status"}) || !tool.IsReadOnly(map[string]any{"action": "list"}) {
		t.Fatal("inspection actions must remain usable in plan mode")
	}
	if tool.IsReadOnly(map[string]any{"action": "create"}) || tool.IsReadOnly(map[string]any{"action": "claim_next"}) {
		t.Fatal("durable state transitions must not be read-only")
	}
}

func TestProjectCoordinatorAssignsFrozenJobThroughHostResolver(t *testing.T) {
	workspace := t.TempDir()
	canonical, err := execution.CanonicalWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	jobs := principal.NewStore(t.TempDir())
	job, err := jobs.Create("role", principal.Definition{Workspace: canonical, Provider: "test", ProviderConfigSHA256: strings.Repeat("a", 64), Profile: principal.Profile{Name: "role", Model: "model", PermissionMode: "plan", Source: "project", Tools: []string{"Read"}}})
	if err != nil {
		t.Fatal(err)
	}
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "dispatch", Items: []projectcoord.WorkItemInput{{ID: "initial", Subject: "initial", Prompt: "initial"}}})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewProjectCoordinator(permission.New(permission.ModeBypass), store, workspace)
	args := map[string]any{"action": "add_work", "run_id": run.ID, "item_id": "assigned", "subject": "assigned", "prompt": "do work", "job_id": job.ID}
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("job accepted without host resolver")
	}
	resolverCalls := 0
	tool = tool.WithPrincipalResolver(func(ctx context.Context, workspace, id string) (*principal.Binding, error) {
		resolverCalls++
		if id != job.ID {
			t.Fatal("wrong ID")
		}
		b := job.Binding()
		return &b, nil
	})
	args["agent_profile"] = "other"
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("conflicting profile accepted")
	}
	delete(args, "agent_profile")
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, job.ID) || resolverCalls != 2 {
		t.Fatal("host binding not visible")
	}
	loaded, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found projectcoord.WorkItem
	for _, item := range loaded.Items {
		if item.ID == "assigned" {
			found = item
		}
	}
	if found.PrincipalBinding == nil || found.PrincipalBinding.ConfigSHA256 != job.ConfigSHA256 || found.AgentProfile != "role" {
		t.Fatal("role definition not frozen")
	}
	// Mutating the resolver's source after admission cannot alter the stored role.
	job.Definition.Profile.Tools[0] = "Write"
	loaded, err = store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Items[1].PrincipalBinding.Definition.Profile.Tools[0] != "Read" {
		t.Fatal("frozen role mutated")
	}
}
