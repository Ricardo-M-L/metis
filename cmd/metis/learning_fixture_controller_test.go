package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

func TestLearningControllerFreezesWorkflowCodeFixtureForWatch(t *testing.T) {
	_, home := learningControllerHostFixture(t)
	suite := learning.Suite{Name: "repair", Cases: []learning.Case{{ID: "repair", Prompt: "repair code", ExpectedJSON: json.RawMessage(`{"tests_passed":true}`)}}, MaxTokens: 1000, MaxDurationSeconds: 30}
	suiteData, _ := json.Marshal(suite)
	base := t.TempDir()
	suitePath := filepath.Join(base, "suite.json")
	fixturePath := filepath.Join(base, "fixture.json")
	fixtureData := workflowFixtureJSON(t)
	for path, data := range map[string][]byte{suitePath: suiteData, fixturePath: fixtureData} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manual, err := loadWorkflowLearningFixtures(fixturePath, suite)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdLearningController(context.Background(), []string{"configure", "--kind", "workflow", "--name", "repair", "--suite", suitePath, "--fixture", fixturePath, "--max-evaluations", "2", "--max-total-tokens", "4000", "--auto-promote"}); err != nil {
		t.Fatal(err)
	}
	st, err := learning.NewController(filepath.Join(home, "learning-controller")).Read()
	if err != nil {
		t.Fatal(err)
	}
	p := st.Policies["workflow/repair"]
	frozen, err := decodeWorkflowLearningFixtures(p.WorkflowFixture, p.Suite)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.SHA256 != manual.SHA256 {
		t.Fatalf("frozen fixture identity changed on persistence: %s %s", frozen.SHA256, manual.SHA256)
	}
	if !p.AutoPromote || len(p.WorkflowFixture) == 0 {
		t.Fatal("fixture policy lost its checker or promotion policy")
	}
	// The watcher consumes immutable stored content, not mutable host paths.
	if err := os.Remove(suitePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixturePath); err != nil {
		t.Fatal(err)
	}
	if err := frozen.ValidateInputs(); err != nil {
		t.Fatal(err)
	}
	if err := cmdLearningController(context.Background(), []string{"watch", "--kind", "workflow", "--name", "repair", "--once"}); err != nil {
		t.Fatal(err)
	}
}

func TestLearningControllerRejectsSkillCodeFixture(t *testing.T) {
	_, _ = learningControllerHostFixture(t)
	data, _ := json.Marshal(validLearningSuiteFixture())
	base := t.TempDir()
	suitePath := filepath.Join(base, "suite.json")
	fixturePath := filepath.Join(base, "fixture.json")
	if err := os.WriteFile(suitePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, workflowFixtureJSON(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdLearningController(context.Background(), []string{"configure", "--kind", "skill", "--name", "repair", "--suite", suitePath, "--fixture", fixturePath, "--max-evaluations", "2", "--max-total-tokens", "400000"}); err == nil {
		t.Fatal("workflow code execution capability granted to skill evaluator")
	}
}
