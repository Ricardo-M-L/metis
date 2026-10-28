package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func TestCoordinatorRunOutputDisclosesAcceptanceConfigurationBeforeWork(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Durable project"})
	if err != nil {
		t.Fatal(err)
	}
	output := coordinatorRunOutputForReadinessTest(t, run)
	for _, want := range []string{"Host acceptance checks: 0", "Host acceptance configuration: missing", "verification cannot complete", "metis coordinator configure", run.ID, "--cwd", "--check '<HOST_ACCEPTANCE_COMMAND>'"} {
		if !strings.Contains(output, want) {
			t.Fatalf("run output omitted early configuration guidance %q: %s", want, output)
		}
	}
	if strings.Contains(output, "project acceptance failed") {
		t.Fatal("unconfigured project was presented as a failed execution")
	}
	run, err = store.SetChecks(workspace, run.ID, []string{"printf host-secret-marker >/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	output = coordinatorRunOutputForReadinessTest(t, run)
	if !strings.Contains(output, "Host acceptance configuration: configured") || !strings.Contains(output, "configuration is not an acceptance result") || strings.Contains(output, "host-secret-marker") || strings.Contains(output, "--check '<HOST_ACCEPTANCE_COMMAND>'") {
		t.Fatalf("configured run output is misleading or leaked a check: %s", output)
	}
}

func TestCoordinatorReadOnlyRunOutputAvoidsIrrelevantConfigurationWarning(t *testing.T) {
	run := projectcoord.Run{ID: "research", Items: []projectcoord.WorkItem{{ID: "inspect", Phase: projectcoord.PhaseResearch}}}
	output := coordinatorRunOutputForReadinessTest(t, run)
	if !strings.Contains(output, "Host acceptance configuration: not_required") || !strings.Contains(output, "no verification phase") || strings.Contains(output, "metis coordinator configure") {
		t.Fatalf("read-only graph acquired irrelevant acceptance warning: %s", output)
	}
}

func coordinatorRunOutputForReadinessTest(t *testing.T, run projectcoord.Run) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "coordinator-output")
	if err != nil {
		t.Fatal(err)
	}
	printCoordinatorRun(file, run)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
