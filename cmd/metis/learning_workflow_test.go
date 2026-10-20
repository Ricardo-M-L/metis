package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

func TestWorkflowLearningTraceGradesActualStepsAndExactVersion(t *testing.T) {
	version := strings.Repeat("a", 64)
	wf := workflow.Workflow{Name: "arith", Steps: []workflow.Step{{Name: "write", Command: `printf '{"answer":%s}' "$((17 * 19))" > answer.json`}, {Name: "read", Command: "cat answer.json"}}}
	record := workflow.RunRecord{Name: "arith", RecipeSHA256: version, Steps: []workflow.StepResult{{Name: "write", Command: wf.Steps[0].Command, Status: workflow.StatusOK, ExitCode: 0}, {Name: "read", Command: wf.Steps[1].Command, Status: workflow.StatusOK, ExitCode: 0, Output: `{"answer":323}`}}}
	rows := workflowTraceRows(t, record)
	path := writeWorkflowTrace(t, rows)
	e, out, err := readWorkflowLearningTrace(path, "arith", version, wf)
	if err != nil || out != `{"answer":323}` || len(e.WorkflowInvocations) != 1 || e.WorkflowInvocations[0].RecipeSHA256 != version || e.WorkflowInvocations[0].ResultSHA256 != learning.Digest([]byte(e.WorkflowInvocations[0].Result)) {
		t.Fatalf("actual trace failed: %+v %q %v", e, out, err)
	}
	for _, mutation := range []string{"wrong-version", "wrong-command", "step-failure", "inline", "unpaired", "duplicate", "different-tool"} {
		t.Run(mutation, func(t *testing.T) {
			copyRows := append([]session.TraceEvent(nil), rows...)
			changed := record
			changed.Steps = append([]workflow.StepResult(nil), record.Steps...)
			switch mutation {
			case "wrong-version":
				changed.RecipeSHA256 = strings.Repeat("b", 64)
			case "wrong-command":
				changed.Steps[0].Command = "echo spoofed"
			case "step-failure":
				changed.Steps[1].ExitCode = 1
			case "inline":
				copyRows[0].Text = `{"operation":"run","name":"arith"}`
			case "unpaired":
				copyRows[1].TraceCallID = "different"
			case "duplicate":
				copyRows = append(copyRows, copyRows[0], copyRows[1])
			case "different-tool":
				copyRows[0].ToolName = "Bash"
			}
			if mutation == "wrong-version" || mutation == "wrong-command" || mutation == "step-failure" {
				data, _ := json.Marshal(changed)
				copyRows[1].Text = string(data)
			}
			if _, _, err := readWorkflowLearningTrace(writeWorkflowTrace(t, copyRows), "arith", version, wf); err == nil {
				t.Fatal("invalid trace accepted")
			}
		})
	}
}

func workflowTraceRows(t *testing.T, record workflow.RunRecord) []session.TraceEvent {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return []session.TraceEvent{{ID: "start", Kind: "tool_start", ToolName: "Workflow", TraceCallID: "call", TraceInvocationID: "invoke", Text: `{"operation":"run_named","name":"arith"}`}, {ID: "result", Kind: "tool_result", ToolName: "Workflow", TraceCallID: "call", TraceInvocationID: "invoke", Text: string(data)}, {ID: "tokens", Kind: "tokens"}, {ID: "assistant", Kind: "text", Text: `{"answer":999}`}}
}
func writeWorkflowTrace(t *testing.T, rows []session.TraceEvent) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkflowLearningEvaluatorPinsProviderProfile(t *testing.T) {
	if d := sandbox.Doctor(); !d.Available {
		t.Skip(d.Err)
	}
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := &config.Config{Provider: config.ProviderSet{Default: "anthropic", Anthropic: config.ProviderAnthropic{APIKey: "unit-test-key", Model: "unit-test-model", BaseURL: "https://initial.test", MaxTokens: 1024}}}
	write := func() {
		var b strings.Builder
		if err := toml.NewEncoder(&b).Encode(cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	loaded, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := newWorkflowLearningQueryRunner(loaded, "arith", learning.Suite{})
	if err != nil {
		t.Fatal(err)
	}
	profile, _, err := learningProviderConfig(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProfileSHA256 != learningProfileDigest(profile) || len(identity.ProfileSHA256) != 64 {
		t.Fatalf("profile not pinned: %+v", identity)
	}
	if err := identity.ValidateInputs(); err != nil {
		t.Fatalf("unchanged evaluator rejected: %v", err)
	}
	cfg.Provider.Anthropic.BaseURL = "https://changed.test"
	write()
	if err := identity.ValidateInputs(); err == nil {
		t.Fatal("provider route drift accepted")
	}
}

func TestWorkflowLearningTracePreservesLargeCompleteResult(t *testing.T) {
	version := strings.Repeat("a", 64)
	wf := workflow.Workflow{Name: "arith", Steps: []workflow.Step{{Name: "large", Command: "echo large"}}}
	body := strings.Repeat("x", 1200000)
	record := workflow.RunRecord{Name: "arith", RecipeSHA256: version, Steps: []workflow.StepResult{{Name: "large", Command: wf.Steps[0].Command, Status: workflow.StatusOK, ExitCode: 0, Output: body}}}
	e, out, err := readWorkflowLearningTrace(writeWorkflowTrace(t, workflowTraceRows(t, record)), "arith", version, wf)
	if err != nil || out != body || len(e.WorkflowInvocations) != 1 {
		t.Fatalf("complete bounded result rejected: out=%d %v", len(out), err)
	}
}
