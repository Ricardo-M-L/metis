package desktopipc

import (
	"encoding/json"
	"testing"
)

func TestContextPressurePreservesDefaultOutputBudgetSeparatelyFromCapability(t *testing.T) {
	var pressure ContextPressure
	if err := json.Unmarshal([]byte(`{"used":1000,"window":1048576,"maxOutputTokens":393216,"defaultOutputBudgetTokens":65536}`), &pressure); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pressure)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["defaultOutputBudgetTokens"] != float64(65536) || wire["maxOutputTokens"] != float64(393216) {
		t.Fatalf("per-response default and model capability were lost or conflated: %s", raw)
	}
	var old ContextPressure
	if err := json.Unmarshal([]byte(`{"used":1000,"window":1048576,"maxOutputTokens":393216}`), &old); err != nil {
		t.Fatal(err)
	}
	oldRaw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var oldWire map[string]any
	if err := json.Unmarshal(oldRaw, &oldWire); err != nil {
		t.Fatal(err)
	}
	if value, found := oldWire["defaultOutputBudgetTokens"]; found && value != float64(0) {
		t.Fatalf("old worker invented an output budget: %s", oldRaw)
	}
}

func TestSubagentContinuingStateAndAttemptsSurviveIPC(t *testing.T) {
	var status Status
	if err := json.Unmarshal([]byte(`{"agents":[{"agentId":"agt-budget","status":"running","executionPhase":"continuing","continuationAttempts":2}]}`), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 1 || status.Agents[0].ExecutionPhase != "continuing" || status.Agents[0].ContinuationAttempts != 2 {
		t.Fatalf("worker continuation state lost: %+v", status.Agents)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Status
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Agents) != 1 || decoded.Agents[0].ExecutionPhase != "continuing" || decoded.Agents[0].ContinuationAttempts != 2 {
		t.Fatalf("continuation state lost on wire: %s", raw)
	}
}
