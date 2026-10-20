package learning

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkflowFixtureEvidenceSurvivesIndentedPersistence(t *testing.T) {
	version := strings.Repeat("a", 64)
	output := "{\"tests_passed\":true}\n"
	result, _ := json.Marshal(map[string]any{"name": "repair", "recipe_sha256": version, "steps": []map[string]any{{"status": "ok", "exit_code": 0, "output": output}}})
	report, _ := json.Marshal(map[string]any{"fixture_sha256": version, "tests_sha256": version, "tests_passed": true, "test_exit_code": 0, "artifacts_passed": true, "source_sha256": map[string]string{"calc.py": version}, "artifact_sha256": map[string]string{"result.json": version}})
	r := CaseResult{Output: output, WorkflowInvoked: true, ModelRequests: 1, Evidence: QueryEvidence{Provider: "test-provider", Model: "test-model", SessionID: "test-session", ManifestSHA256: version, TraceSHA256: version, TokenEventIDs: []string{"token-1"}, FixtureSHA256: version, HostChecksSHA256: EvaluationFixtureDigest(report), HostChecksPassed: true, HostChecksReport: report, WorkflowInvocations: []SkillInvocation{{InvocationID: "invoke-1", CallID: "call-1", Arguments: json.RawMessage(`{"operation":"run_named","name":"repair"}`), Result: string(result), ResultSHA256: Digest(result), RecipeSHA256: version}}}}
	if !validEvidence(r, version, "test-provider", "test-model", "repair", "workflow") {
		t.Fatal("initial fixture evidence rejected")
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var persisted CaseResult
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if string(persisted.Evidence.HostChecksReport) == string(report) {
		t.Fatal("test did not exercise RawMessage indentation changes")
	}
	if !validEvidence(persisted, version, "test-provider", "test-model", "repair", "workflow") {
		t.Fatal("persisted fixture lost its canonical evidence binding")
	}
	persisted.Evidence.HostChecksReport = json.RawMessage(strings.ReplaceAll(string(persisted.Evidence.HostChecksReport), `"test_exit_code": 0`, `"test_exit_code": 1`))
	if validEvidence(persisted, version, "test-provider", "test-model", "repair", "workflow") {
		t.Fatal("changed host assertion evidence accepted")
	}
}

func TestEvaluationFixtureDigestCanonicalizesPolicyEscapes(t *testing.T) {
	initial := []byte(`{"test_python":"if a > 0 and a < 4: print('safe')","cases":{"b":2,"a":1}}`)
	escaped := []byte(` { "cases": {"a":1,"b":2}, "test_python":"if a \u003e 0 and a \u003c 4: print('safe')" } `)
	if EvaluationFixtureDigest(initial) == "" || EvaluationFixtureDigest(initial) != EvaluationFixtureDigest(escaped) {
		t.Fatal("policy escaping or field order changed frozen fixture identity")
	}
}
