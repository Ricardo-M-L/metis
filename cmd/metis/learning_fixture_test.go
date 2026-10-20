package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

func workflowFixtureJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(workflow.EvaluationFixtures{SchemaVersion: 1, Cases: map[string]workflow.EvaluationFixture{"repair": {
		Files: map[string]string{"calc.py": "def multiply(a,b): return a+b\n"}, WritableFiles: []string{"calc.py"},
		TestPython:        "import json\nwith open('result.json','w') as f: json.dump({'answer':323},f)\n",
		ExpectedArtifacts: map[string]json.RawMessage{"result.json": json.RawMessage(`{"answer":323}`)},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWorkflowLearningFixtureBindsCaseIDsAndImmutableInputs(t *testing.T) {
	suite := learning.Suite{Name: "repair", Cases: []learning.Case{{ID: "repair", Prompt: "repair code", ExpectedJSON: json.RawMessage(`{"tests_passed":true}`)}}, MaxTokens: 1000, MaxDurationSeconds: 30}
	if f, err := loadWorkflowLearningFixtures("", suite); err != nil || f != nil {
		t.Fatalf("default policy changed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	data := workflowFixtureJSON(t)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := loadWorkflowLearningFixtures(path, suite)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ValidateInputs(); err != nil {
		t.Fatal(err)
	}
	profile := strings.Repeat("a", 64)
	if workflowLearningFixtureProfileDigest(profile, nil) != profile || workflowLearningFixtureProfileDigest(profile, f) == profile {
		t.Fatal("fixture identity was not bound separately from the default policy")
	}
	suite.Cases[0].ID = "different"
	if _, err := loadWorkflowLearningFixtures(path, suite); err == nil {
		t.Fatal("unmatched fixture case accepted")
	}
	if err := os.WriteFile(path, append(data, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.ValidateInputs(); err == nil {
		t.Fatal("fixture changed after configuration")
	}
}

func TestWorkflowLearningFixtureTraceRetainsRealFailingBaseline(t *testing.T) {
	version := strings.Repeat("a", 64)
	wf := workflow.Workflow{Name: "arith", Steps: []workflow.Step{{Name: "test-code", Command: "fixture-test"}}}
	record := workflow.RunRecord{Name: "arith", RecipeSHA256: version, Steps: []workflow.StepResult{{Name: "test-code", Command: "fixture-test", Status: workflow.StatusFailed, ExitCode: 1, Output: "host fixture tests failed (exit 1): AssertionError: 36 != 323"}}}
	rows := workflowTraceRows(t, record)
	rows[1].IsError = true
	path := writeWorkflowTrace(t, rows)
	e, output, err := readWorkflowLearningFixtureTrace(path, "arith", version, wf, true)
	if err != nil || len(e.WorkflowInvocations) != 1 || !strings.Contains(output, "AssertionError") {
		t.Fatalf("completed failing baseline was lost: %+v %q %v", e, output, err)
	}
	if _, _, err := readWorkflowLearningTrace(path, "arith", version, wf); err == nil {
		t.Fatal("fixture opt-in weakened default evaluation")
	}
	// Malformed/non-paired results still interrupt comparison rather than being
	// counted as an ordinary failing business case.
	rows[1].TraceCallID = "other"
	if _, _, err := readWorkflowLearningFixtureTrace(writeWorkflowTrace(t, rows), "arith", version, wf, true); err == nil {
		t.Fatal("unpaired failing baseline accepted")
	}
	rows = append(rows, session.TraceEvent{Kind: "tool_start", ToolName: "Bash"})
	if _, _, err := readWorkflowLearningFixtureTrace(writeWorkflowTrace(t, rows), "arith", version, wf, true); err == nil {
		t.Fatal("different tool accepted")
	}
}
