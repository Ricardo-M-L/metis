package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

func TestWorkflowLearningActivationAndRollbackAuthority(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir())
	baseline := Workflow{Name: "arith", Steps: []Step{{Name: "answer", Command: `printf '{"answer":322}'`}}}
	if err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	candidate := Workflow{Name: "arith", Steps: []Step{{Name: "answer", Command: `printf '{"answer":%s}' "$((17 * 19))"`}}}
	v, err := store.Propose(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.LearningStore().Read("arith")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load("arith"); err != nil || got.Steps[0].Command != baseline.Steps[0].Command {
		t.Fatalf("candidate replaced baseline: %+v %v", got, err)
	}
	if err := store.LearningStore().Promote(ctx, "arith", v.ID, "unmeasured"); err == nil {
		t.Fatal("unmeasured candidate promoted")
	}
	suite := learning.Suite{Name: "host-arithmetic", Cases: []learning.Case{{ID: "a", Prompt: "run arithmetic", ExpectedJSON: json.RawMessage(`{"answer":323}`)}}, MaxTokens: 1000, MaxDurationSeconds: 60}
	runner := func(_ context.Context, version learning.Version, c learning.Case) (learning.CaseResult, error) {
		wf, err := ParseManifest("arith", []byte(version.Content))
		if err != nil {
			return learning.CaseResult{}, err
		}
		answer := `{"answer":322}`
		if version.ID == v.ID {
			answer = `{"answer":323}`
		}
		record, _ := json.Marshal(RunRecord{Name: "arith", RecipeSHA256: version.ID, Steps: []StepResult{{Name: wf.Steps[0].Name, Command: wf.Steps[0].Command, Status: StatusOK, ExitCode: 0, Output: answer}}})
		arguments := json.RawMessage(`{"operation":"run_named","name":"arith"}`)
		return learning.CaseResult{ID: c.ID, Output: answer, ExitCode: 0, WorkflowInvoked: true, ModelRequests: 1, UsageReported: true, Usage: learning.Usage{Input: 10, Output: 5}, Evidence: learning.QueryEvidence{Provider: "test", Model: "test", SessionID: version.ID, ManifestSHA256: version.ID, TraceSHA256: strings.Repeat("b", 64), TokenEventIDs: []string{"tokens"}, WorkflowInvocations: []learning.SkillInvocation{{InvocationID: "invoke", CallID: "call", Arguments: arguments, Result: string(record), ResultSHA256: learning.Digest(record), RecipeSHA256: version.ID}}}}, nil
	}
	e, err := store.LearningStore().Evaluate(ctx, "arith", v.ID, suite, learning.EvaluatorIdentity{Kind: "workflow", ProfileSHA256: strings.Repeat("c", 64), BinarySHA256: strings.Repeat("a", 64), Provider: "test", Model: "test"}, runner)
	if err != nil || !e.Passed {
		t.Fatalf("evaluate: %+v %v", e, err)
	}
	if err := store.LearningStore().Promote(ctx, "arith", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load("arith"); err != nil || got.VersionSHA256 != v.ID || got.Steps[0].Command != candidate.Steps[0].Command {
		t.Fatalf("promoted recipe not consumed: %+v %v", got, err)
	}
	// Changing the legacy JSON cannot replace the activation authority.
	if err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load("arith"); err != nil || got.VersionSHA256 != v.ID {
		t.Fatalf("legacy overwrite bypassed active version: %+v %v", got, err)
	}
	if err := store.LearningStore().Rollback(ctx, "arith", st.BaselineVersion); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load("arith"); err != nil || got.Steps[0].Command != baseline.Steps[0].Command || got.VersionSHA256 != st.BaselineVersion {
		t.Fatalf("rollback not consumed: %+v %v", got, err)
	}
}

func TestWorkflowLearningNewCandidateCannotRunAndCorruptionFailsClosed(t *testing.T) {
	store := NewStore(t.TempDir())
	wf := Workflow{Name: "pending", Steps: []Step{{Name: "answer", Command: "echo hello"}}}
	if _, err := store.Propose(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("pending"); err == nil {
		t.Fatal("new pending candidate runnable")
	}
	if err := store.Save(wf); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("pending"); err == nil {
		t.Fatal("new pending candidate bypassed host evaluation")
	}
	path := filepath.Join(store.Dir(), ".learning", "pending", "state.json")
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("pending"); err == nil {
		t.Fatal("corrupt learning authority fell back to raw recipe")
	}
}

func TestWorkflowManifestRejectsAmbiguousRecipe(t *testing.T) {
	for _, content := range []string{`{"name":"wrong","steps":[{"name":"a","command":"true"}]}`, `{"name":"demo","steps":[]}`, `{"name":"demo","steps":[{"name":"a","command":"true"},{"name":"a","command":"false"}]}`, `{"name":"demo","steps":[{"name":"a","command":"true"}],"version":"approved"}`, `{"name":"demo","steps":[{"name":"a","command":"true"}]} {}`} {
		if _, err := ParseManifest("demo", []byte(content)); err == nil {
			t.Errorf("manifest accepted: %s", content)
		}
	}
}
