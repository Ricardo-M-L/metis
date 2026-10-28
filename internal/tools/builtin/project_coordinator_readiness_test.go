package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

type coordinatorReadinessPayload struct {
	Run struct {
		ID             string `json:"id"`
		Workspace      string `json:"workspace"`
		HostCheckCount int    `json:"host_check_count"`
		HostAcceptance struct {
			Status            string               `json:"status"`
			RequiredFor       []projectcoord.Phase `json:"required_for"`
			ConfigurationHint string               `json:"configuration_hint"`
			Summary           string               `json:"summary"`
		} `json:"host_acceptance"`
	} `json:"run"`
}

func TestProjectCoordinatorAdvertisesMissingHostChecksBeforeDispatch(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "project with ' quotes")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	store := projectcoord.NewStore(t.TempDir())
	tool := NewProjectCoordinator(permission.New(permission.ModeBypass), store, workspace)
	created, err := tool.Execute(context.Background(), map[string]any{"action": "create", "goal": "Durable implementation"})
	if err != nil {
		t.Fatal(err)
	}
	var payload coordinatorReadinessPayload
	if err := json.Unmarshal([]byte(created.Output), &payload); err != nil {
		t.Fatal(err)
	}
	readiness := payload.Run.HostAcceptance
	if readiness.Status != "missing" || payload.Run.HostCheckCount != 0 {
		t.Fatalf("create did not disclose missing acceptance configuration: %s", created.Output)
	}
	if len(readiness.RequiredFor) != 1 || readiness.RequiredFor[0] != projectcoord.PhaseVerification {
		t.Fatalf("missing gate is not scoped to verification: %+v", readiness)
	}
	for _, part := range []string{"metis coordinator configure", shellQuote(payload.Run.ID), "--cwd " + shellQuote(payload.Run.Workspace), "--check '<HOST_ACCEPTANCE_COMMAND>'"} {
		if !strings.Contains(readiness.ConfigurationHint, part) {
			t.Fatalf("configuration hint omitted %q: %q", part, readiness.ConfigurationHint)
		}
	}
	if !strings.Contains(strings.ToLower(readiness.Summary), "host") || !strings.Contains(strings.ToLower(readiness.Summary), "verification") {
		t.Fatalf("configuration summary does not explain the blocking phase: %q", readiness.Summary)
	}
	for _, action := range []string{"status", "list", "claim_next"} {
		result, err := tool.Execute(context.Background(), map[string]any{"action": action, "run_id": payload.Run.ID, "worker": "researcher"})
		if err != nil {
			t.Fatalf("research setup/claim should remain possible without checks (%s): %v", action, err)
		}
		if !strings.Contains(result.Output, `"host_acceptance"`) || !strings.Contains(result.Output, `"status": "missing"`) {
			t.Fatalf("%s hides missing acceptance configuration: %s", action, result.Output)
		}
	}
}

func TestProjectCoordinatorReadinessRefreshesAfterHostConfiguration(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Configure a durable project"})
	if err != nil {
		t.Fatal(err)
	}
	const sensitiveCheck = "printf host-secret-marker >/dev/null"
	if _, err := store.SetChecks(workspace, run.ID, []string{sensitiveCheck}); err != nil {
		t.Fatal(err)
	}
	tool := NewProjectCoordinator(permission.New(permission.ModeBypass), store, workspace)
	for _, action := range []string{"status", "list", "claim_next"} {
		result, err := tool.Execute(context.Background(), map[string]any{"action": action, "run_id": run.ID, "worker": "researcher"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Output, `"status": "configured"`) || strings.Contains(result.Output, `"configuration_hint"`) || strings.Contains(result.Output, "host-secret-marker") {
			t.Fatalf("%s did not refresh or leaked configured check: %s", action, result.Output)
		}
		if action != "claim_next" && !strings.Contains(result.Output, `"host_check_count": 1`) {
			t.Fatalf("%s omitted existing backwards-compatible check count: %s", action, result.Output)
		}
	}
}

func TestProjectCoordinatorReadOnlyGraphDoesNotPretendToPassAcceptance(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{
		Workspace: workspace, Goal: "Read-only durable research",
		Items: []projectcoord.WorkItemInput{{ID: "research", Phase: projectcoord.PhaseResearch, Subject: "Research", Prompt: "Read a document"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewProjectCoordinator(permission.New(permission.ModeBypass), store, workspace)
	status, err := tool.Execute(context.Background(), map[string]any{"action": "status", "run_id": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	var payload coordinatorReadinessPayload
	if err := json.Unmarshal([]byte(status.Output), &payload); err != nil {
		t.Fatal(err)
	}
	readiness := payload.Run.HostAcceptance
	if readiness.Status != "not_required" || len(readiness.RequiredFor) != 0 || readiness.ConfigurationHint != "" || !strings.Contains(readiness.Summary, "verification phase") {
		t.Fatalf("read-only graph gained irrelevant warning or claimed acceptance: %+v", readiness)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"action": "add_work", "run_id": run.ID, "item_id": "verify", "phase": "verification", "subject": "Verify", "prompt": "Verify the deliverable",
	}); err != nil {
		t.Fatal(err)
	}
	status, err = tool.Execute(context.Background(), map[string]any{"action": "status", "run_id": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Output, `"status": "missing"`) {
		t.Fatalf("adding a verification phase did not update readiness: %s", status.Output)
	}
}

func TestProjectCoordinatorReadinessCannotReplaceHostVerification(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Verify", Items: []projectcoord.WorkItemInput{{ID: "verify", Phase: projectcoord.PhaseVerification, Subject: "Verify", Prompt: "Verify"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewProjectCoordinator(permission.New(permission.ModeBypass), store, workspace)
	args := map[string]any{
		"action": "complete", "run_id": run.ID, "item_id": item.ID, "worker": item.Owner, "lease_token": item.Lease.Token,
		"output": "PASS", "check": "exit 0", "check_commands": []any{"exit 0"}, "skip_checks": true,
	}
	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "needs host acceptance checks") {
		t.Fatalf("model supplied PASS/checks bypassed host gate: %v", err)
	}
	if _, err := store.SetChecks(workspace, run.ID, []string{"exit 9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("configured but failed host checks were accepted")
	}
	if _, err := store.SetChecks(workspace, run.ID, []string{"exit 0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("passing host check did not permit verification: %v", err)
	}
	after, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != projectcoord.RunCompleted || after.Items[0].Acceptance == nil || after.Items[0].Acceptance.Status != "passed" || len(after.Items[0].Acceptance.Checks) != 1 || after.Items[0].Acceptance.Checks[0].ExitCode != 0 {
		t.Fatalf("host verification evidence not preserved: %+v", after.Items[0].Acceptance)
	}
}
