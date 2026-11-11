package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type agentOutputRecoveryProvider struct {
	mu        sync.Mutex
	budget    int
	segments  [][]llm.StreamEvent
	requests  []llm.Request
	failureAt int
	failure   error
}

func (*agentOutputRecoveryProvider) Name() string               { return "output-recovery-test" }
func (*agentOutputRecoveryProvider) ModelID() string            { return "output-model" }
func (*agentOutputRecoveryProvider) MaxContextTokens() int      { return 100_000 }
func (p *agentOutputRecoveryProvider) DefaultOutputBudget() int { return p.budget }
func (*agentOutputRecoveryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not used")
}
func (p *agentOutputRecoveryProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := len(p.requests)
	p.requests = append(p.requests, req)
	if i+1 == p.failureAt {
		return nil, p.failure
	}
	if i >= len(p.segments) {
		return nil, errors.New("unexpected extra request")
	}
	return &fakeStream{events: p.segments[i]}, nil
}
func (p *agentOutputRecoveryProvider) captured() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}
func agentRecoverySegment(text, stop string) []llm.StreamEvent {
	return []llm.StreamEvent{{Type: "text_delta", TextDelta: text}, {Type: "message_delta", StopReason: stop}, {Type: "message_stop", StopReason: stop, InputTokens: 10, OutputTokens: 20}}
}

func TestAgentOutputRecoveryForegroundAndBackgroundUseSameDurableRunner(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			provider := &agentOutputRecoveryProvider{budget: 4096, segments: [][]llm.StreamEvent{agentRecoverySegment("FIRST_PART ", "max_tokens"), agentRecoverySegment("FINAL_TAIL", "end_turn")}}
			dir := t.TempDir()
			roster := agent.NewRoster(2)
			tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "output-model", "system").WithRoster(roster).WithSessionPersistence(dir, "output-parent")
			result, err := tool.Execute(context.Background(), map[string]any{"prompt": "answer in full", "run_in_background": background, "max_output_tokens": 1024})
			if err != nil || result == nil || result.IsError {
				t.Fatalf("Execute=%#v, %v", result, err)
			}
			meta, _ := result.Presentation["subagent"].(map[string]any)
			id, _ := meta["agentId"].(string)
			if id == "" || result.Presentation[agent.AgentStartedPresentationKey] != true {
				t.Fatalf("missing started identity: %#v", result)
			}
			var saved *agent.SubAgentSnapshot
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				saved, err = agent.LoadSubAgentSnapshot(dir, id)
				if err == nil && saved.Terminal != nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err != nil || saved == nil || saved.Terminal == nil {
				t.Fatalf("missing terminal: %#v %v", saved, err)
			}
			if saved.Terminal.Status != "completed" || saved.Terminal.StopHint != "end_turn" || saved.Terminal.Output != "FIRST_PART FINAL_TAIL" || saved.Terminal.ContinuationAttempts != 1 {
				t.Fatalf("terminal=%#v", saved.Terminal)
			}
			if saved.Execution == nil || saved.Execution.MaxOutputTokens != 1024 {
				t.Fatalf("child output budget not saved: %#v", saved.Execution)
			}
			requests := provider.captured()
			if len(requests) != 2 || requests[0].MaxTokens != 1024 || requests[1].MaxTokens != 1024 || provider.budget != 4096 {
				t.Fatalf("child cap mutated or lost: %#v default=%d", requests, provider.budget)
			}
			if !background && (result.Output != "FIRST_PART FINAL_TAIL" || meta["continuationAttempts"] != 1 || meta["partialOutput"] != false) {
				t.Fatalf("foreground output=%#v", result)
			}
		})
	}
}

func TestAgentOutputRecoveryFailureRetainsActualReasonAndPartialForParent(t *testing.T) {
	provider := &agentOutputRecoveryProvider{budget: 4096, segments: [][]llm.StreamEvent{agentRecoverySegment("PART_A", "max_tokens"), agentRecoverySegment("PART_B", "length"), agentRecoverySegment("PART_C", "max_tokens"), agentRecoverySegment("PART_D", "length")}}
	dir := t.TempDir()
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "output-model", "system").WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "output-parent")
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "answer in full"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("expected incomplete failure: %#v %v", result, err)
	}
	meta, _ := result.Presentation["subagent"].(map[string]any)
	id, _ := meta["agentId"].(string)
	if meta["stopReason"] != "length" || meta["continuationAttempts"] != 3 || meta["partialOutput"] != true || meta["canResume"] != true || meta["diagnostic"] == "" || result.Presentation[agent.AgentStartedPresentationKey] != true {
		t.Fatalf("wrong parent classification: %#v", result)
	}
	if !strings.Contains(result.Output, "agent_id="+id) || !strings.Contains(result.Output, "stop_reason=length") || !strings.Contains(result.Output, "PART_APART_BPART_CPART_D") || !strings.Contains(result.Output, "not a completed result") {
		t.Fatalf("partial failure details lost: %q", result.Output)
	}
	saved, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || saved.Terminal == nil || saved.Terminal.Status != "failed" || saved.Terminal.StopHint != "length" || saved.Terminal.ContinuationAttempts != 3 {
		t.Fatalf("terminal=%#v %v", saved, err)
	}
}

func TestAgentOutputCapRejectsInvalidAndUnsupportedValuesWithoutRequests(t *testing.T) {
	provider := &agentOutputRecoveryProvider{budget: 4096}
	for _, value := range []any{0, -1, 1.5, math.NaN(), math.Inf(1), "1024", nil, json.Number("3.2"), 8192} {
		_, err := agentRequestedOutputCap(map[string]any{"max_output_tokens": value}, provider)
		if err == nil {
			t.Errorf("accepted invalid cap %v", value)
		}
	}
	for _, value := range []any{int(1024), int64(1024), float64(1024), json.Number("1024")} {
		if cap, err := agentRequestedOutputCap(map[string]any{"max_output_tokens": value}, provider); err != nil || cap != 1024 {
			t.Fatalf("cap=%d %v", cap, err)
		}
	}
	if cap, err := agentRequestedOutputCap(map[string]any{}, helloProvider()); err != nil || cap != 0 {
		t.Fatalf("omitted cap changed unknown-provider behavior: %d %v", cap, err)
	}
	if _, err := agentRequestedOutputCap(map[string]any{"max_output_tokens": 1024}, helloProvider()); err == nil || !strings.Contains(err.Error(), "server-managed") {
		t.Fatalf("unsupported explicit cap not refused: %v", err)
	}
}

func TestAgentOutputCapOmissionKeepsProviderManagedRequestUnchanged(t *testing.T) {
	for _, budget := range []int{0, 65536} {
		provider := &agentOutputRecoveryProvider{budget: budget, segments: [][]llm.StreamEvent{agentRecoverySegment("complete", "end_turn")}}
		tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "output-model", "system").WithRoster(agent.NewRoster(2))
		result, err := tool.Execute(context.Background(), map[string]any{"prompt": "complete"})
		requests := provider.captured()
		if err != nil || result.IsError || len(requests) != 1 || requests[0].MaxTokens != 0 || provider.budget != budget {
			t.Fatalf("omitted cap changed managed provider budget=%d: result=%#v err=%v requests=%#v", budget, result, err, requests)
		}
	}
}

func TestAgentOutputCapColdResumePreservesSavedBoundary(t *testing.T) {
	provider := &agentOutputRecoveryProvider{budget: 4096, segments: [][]llm.StreamEvent{agentRecoverySegment("first", "end_turn"), agentRecoverySegment("second", "end_turn")}}
	dir := t.TempDir()
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "output-model", "system").WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "output-parent")
	id := parentContinuationExecute(t, tool, map[string]any{"prompt": "first task", "max_output_tokens": 1024})
	provider.budget = 65536
	if resumed := parentContinuationExecute(t, tool, map[string]any{"resume_from": id, "prompt": "continue"}); resumed != id {
		t.Fatalf("resumed new ID=%q", resumed)
	}
	requests := provider.captured()
	if len(requests) != 2 || requests[0].MaxTokens != 1024 || requests[1].MaxTokens != 1024 {
		t.Fatalf("resume lost child budget: %#v", requests)
	}
	result, err := tool.Execute(context.Background(), map[string]any{"resume_from": id, "prompt": "continue", "max_output_tokens": 2048})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "cannot change max_output_tokens") || len(provider.captured()) != 2 {
		t.Fatalf("resume raised cap: %#v %v", result, err)
	}
	strict := tool
	strict.gate = permission.New(permission.ModePlan)
	result, err = strict.Execute(context.Background(), map[string]any{"resume_from": id, "prompt": "continue"})
	if err != nil || !result.IsError || len(provider.captured()) != 2 {
		t.Fatalf("resume bypassed stricter parent: %#v %v", result, err)
	}
}

func TestAgentFailureDiagnosticNeverCopiesProviderErrorPayload(t *testing.T) {
	secret := "PRIVATE_CREDENTIAL_SENTINEL"
	child := &agent.Teammate{AgentID: "agt-safe", Name: "failed worker", Status: agent.StatusRunning}
	child.AppendText("useful partial")
	child.Finish(agent.StatusFailed, "useful partial", &transport.HTTPStatusError{StatusCode: 400, Err: errors.New(secret)}, "")
	result := decorateAgentSubAgentResult(&tools.Result{IsError: true, Output: secret}, "parent", "call", child)
	encoded, _ := json.Marshal(result.Presentation)
	if strings.Contains(result.Output, secret) || strings.Contains(string(encoded), secret) || !strings.Contains(result.Output, "api_error") || !strings.Contains(result.Output, "useful partial") {
		t.Fatalf("unsafe diagnostic: %#v", result)
	}
}

func TestAgentOutputRecoveryLaterAPIErrorDoesNotKeepOldLengthReason(t *testing.T) {
	provider := &agentOutputRecoveryProvider{budget: 4096, segments: [][]llm.StreamEvent{agentRecoverySegment("retained partial", "max_tokens")}, failureAt: 2, failure: &transport.HTTPStatusError{StatusCode: 400, Err: errors.New("PRIVATE_HTTP_PAYLOAD")}}
	dir := t.TempDir()
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "output-model", "system").WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "output-parent")
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "complete the answer"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("error lost: result=%#v err=%v", result, err)
	}
	meta, _ := result.Presentation["subagent"].(map[string]any)
	id, _ := meta["agentId"].(string)
	if meta["stopReason"] != "api_error" || meta["continuationAttempts"] != 1 || strings.Contains(result.Output, "PRIVATE_HTTP_PAYLOAD") {
		t.Fatalf("wrong final failure cause: %#v", result)
	}
	saved, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || saved.Terminal == nil || saved.Terminal.StopHint != "api_error" || len(provider.captured()) != 2 {
		t.Fatalf("terminal=%#v err=%v requests=%d", saved, err, len(provider.captured()))
	}
}
