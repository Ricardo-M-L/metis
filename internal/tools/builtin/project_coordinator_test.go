package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
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
	if _, err := tool.Execute(context.Background(), map[string]any{
		"action": "complete", "run_id": createPayload.Run.ID, "item_id": "research", "worker": "researcher", "output": "repository inspected",
	}); err != nil {
		t.Fatal(err)
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
