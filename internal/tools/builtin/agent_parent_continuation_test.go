package builtin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type parentContinuationProvider struct {
	name     string
	mu       sync.Mutex
	requests []llm.Request
	entered  chan struct{}
	release  chan struct{}
	toolAt   int
	toolName string
}

func (p *parentContinuationProvider) Name() string        { return p.name }
func (*parentContinuationProvider) ModelID() string       { return "" }
func (*parentContinuationProvider) MaxContextTokens() int { return 100000 }
func (*parentContinuationProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not used")
}
func (p *parentContinuationProvider) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	index := len(p.requests)
	first := index == 1
	p.mu.Unlock()
	if index == p.toolAt {
		return &fakeStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "saved-cwd", ToolName: p.toolName},
			{Type: "tool_input_delta", ToolUseID: "saved-cwd", InputDelta: "{}"},
			{Type: "tool_use_stop", ToolUseID: "saved-cwd", InputDelta: "{}"},
			{Type: "message_delta", StopReason: "tool_use"}, {Type: "message_stop"},
		}}, nil
	}
	stream := &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "child complete"},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}
	if first && p.entered != nil {
		return &parentContinuationGatedStream{ctx: ctx, entered: p.entered, release: p.release, inner: stream}, nil
	}
	return stream, nil
}
func (p *parentContinuationProvider) captured() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

type parentContinuationGatedStream struct {
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	inner   llm.StreamReader
	waiting bool
}

func (s *parentContinuationGatedStream) Recv() (llm.StreamEvent, error) {
	if !s.waiting {
		s.waiting = true
		close(s.entered)
		select {
		case <-s.release:
		case <-s.ctx.Done():
			return llm.StreamEvent{}, s.ctx.Err()
		}
	}
	return s.inner.Recv()
}
func (s *parentContinuationGatedStream) Close() error { return s.inner.Close() }

type parentContinuationCwdTool struct {
	stubTool
	observed chan string
}

func (t parentContinuationCwdTool) Execute(ctx context.Context, input map[string]any) (*tools.Result, error) {
	t.observed <- agent.CwdFromContext(ctx)
	return &tools.Result{Output: "saved directory inspected"}, nil
}

func parentContinuationExecute(t *testing.T, tool Agent, input map[string]any) string {
	t.Helper()
	result, err := tool.Execute(context.Background(), input)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Agent.Execute = %#v, %v", result, err)
	}
	metadata, _ := result.Presentation["subagent"].(map[string]any)
	id, _ := metadata["agentId"].(string)
	if id == "" {
		t.Fatalf("missing stable child ID: %#v", result)
	}
	return id
}

func TestParentContinuationRestoresExecutionAndPreservesAppendHistory(t *testing.T) {
	dir, workDir, nextWorkDir := t.TempDir(), t.TempDir(), t.TempDir()
	provider := &parentContinuationProvider{name: "original-provider", toolAt: 2, toolName: "Read"}
	registry := buildParentRegistry(t, "Write")
	observedCwd := make(chan string, 1)
	registry.Register(parentContinuationCwdTool{stubTool: stubTool{name: "Read"}, observed: observedCwd})
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, registry, "original-model", "original parent prompt").
		WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "original-parent").
		WithProfileLoader(func(string) (*AgentProfileSpec, error) {
			return &AgentProfileSpec{Name: "audit", SystemPrompt: "original audit role", Tools: []string{"Read"}}, nil
		})
	tool.promptProviderName = "configured-original-provider"
	tool.effort = func() llm.Effort { return llm.Effort("high") }
	tool.fast = func() bool { return false }
	id := parentContinuationExecute(t, tool, map[string]any{"prompt": "original task", "subagent_type": "audit", "cwd": workDir})
	snapshot, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || snapshot.Execution == nil {
		t.Fatalf("missing saved execution: %#v, %v", snapshot, err)
	}
	original := *snapshot.Execution
	if original.Provider != "configured-original-provider" || original.Model != "original-model" || original.Effort != "high" || original.WorkDir != workDir || original.Profile != "audit" || !reflect.DeepEqual(original.Tools, []string{"Read"}) {
		t.Fatalf("wrong original execution: %#v", original)
	}
	path := filepath.Join(dir, agent.SubAgentTranscriptDirname, id+".jsonl")
	prefix, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	other := &parentContinuationProvider{name: "different-provider"}
	tool.provider, tool.model, tool.system, tool.promptProviderName = other, "different-model", "changed parent prompt", "different-provider"
	tool.promptWorkDir = nextWorkDir
	tool.effort = func() llm.Effort { return llm.Effort("low") }
	tool.profileLoader = func(string) (*AgentProfileSpec, error) {
		t.Fatal("saved profile was reloaded from mutable configuration")
		return nil, nil
	}
	registry.Register(stubTool{name: "Bash"})
	var resolved [2]string
	tool.resolveProvider = func(providerName, model string) (llm.Provider, error) {
		resolved = [2]string{providerName, model}
		return provider, nil
	}
	if nextID := parentContinuationExecute(t, tool, map[string]any{"resume_from": id, "prompt": "follow-up task"}); nextID != id {
		t.Fatalf("resume created another child: %q != %q", nextID, id)
	}
	if resolved != [2]string{"configured-original-provider", "original-model"} || len(other.captured()) != 0 {
		t.Fatalf("saved provider was replaced: resolved=%#v otherRequests=%d", resolved, len(other.captured()))
	}
	select {
	case actual := <-observedCwd:
		if actual != workDir {
			t.Fatalf("continued tool ran in %q, want original %q", actual, workDir)
		}
	default:
		t.Fatal("continued child did not execute its original Read tool")
	}
	requests := provider.captured()
	last := requests[len(requests)-1]
	if last.Model != "original-model" || string(last.Effort) != "high" || last.System != original.System {
		t.Fatalf("saved model/effort/system changed: model=%q effort=%q system=%q", last.Model, last.Effort, last.System)
	}
	if len(last.Tools) != 1 || last.Tools[0].Name != "Read" {
		t.Fatalf("current parent tools leaked into saved child: %#v", last.Tools)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(after, prefix) || len(after) <= len(prefix) {
		t.Fatalf("cold resume replaced original transcript: prefix=%d after=%d err=%v", len(prefix), len(after), err)
	}
	resumed, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || !reflect.DeepEqual(resumed.Execution, snapshot.Execution) {
		t.Fatalf("saved execution changed: %#v, %v", resumed, err)
	}
}

func TestParentContinuationCannotTransferChildToAnotherParent(t *testing.T) {
	dir := t.TempDir()
	provider := &parentContinuationProvider{name: "provider"}
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "model", "system").
		WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "parent-a")
	id := parentContinuationExecute(t, tool, map[string]any{"prompt": "original task"})
	path := filepath.Join(dir, agent.SubAgentTranscriptDirname, id+".jsonl")
	before, _ := os.ReadFile(path)
	tool.parentSessionID = "parent-b"
	result, err := tool.Execute(context.Background(), map[string]any{"resume_from": id, "prompt": "steal child"})
	after, _ := os.ReadFile(path)
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "original direct parent") || len(provider.captured()) != 1 || !bytes.Equal(before, after) {
		t.Fatalf("cross-parent continuation changed child: result=%#v err=%v requests=%d", result, err, len(provider.captured()))
	}
}

func TestParentContinuationRejectsPermissionEscalationAfterParentRestricts(t *testing.T) {
	for _, modes := range [][2]permission.Mode{{permission.ModeFullAccess, permission.ModeDontAsk}, {permission.ModeBypassPermissions, permission.ModeDefault}} {
		t.Run(string(modes[0])+"_to_"+string(modes[1]), func(t *testing.T) {
			dir := t.TempDir()
			provider := &parentContinuationProvider{name: "provider"}
			tool := NewAgent(permission.New(modes[0]), provider, tools.NewRegistry(), "model", "system").
				WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "parent")
			id := parentContinuationExecute(t, tool, map[string]any{"prompt": "original task"})
			path := filepath.Join(dir, agent.SubAgentTranscriptDirname, id+".jsonl")
			before, _ := os.ReadFile(path)
			tool.gate.SetMode(modes[1])
			result, err := tool.Execute(context.Background(), map[string]any{"resume_from": id, "prompt": "continue"})
			after, _ := os.ReadFile(path)
			if err != nil || result == nil || !result.IsError || len(provider.captured()) != 1 || !bytes.Equal(before, after) {
				t.Fatalf("saved elevated child bypassed current parent: %#v, %v, requests=%d", result, err, len(provider.captured()))
			}
		})
	}
}

func TestParentContinuationPlanChildCannotBeUpgradedByResumeOverride(t *testing.T) {
	dir := t.TempDir()
	provider := &parentContinuationProvider{name: "provider"}
	tool := NewAgent(permission.New(permission.ModePlan), provider, buildParentRegistry(t, "Read", "Write"), "model", "system").
		WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "parent")
	id := parentContinuationExecute(t, tool, map[string]any{"prompt": "inspect only"})
	tool.gate.SetMode(permission.ModeBypassPermissions)
	result, err := tool.Execute(context.Background(), map[string]any{"resume_from": id, "prompt": "write now", "permission_mode": "bypassPermissions"})
	if err != nil || result == nil || !result.IsError || len(provider.captured()) != 1 {
		t.Fatalf("Plan child upgraded: %#v, %v", result, err)
	}
	saved, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || saved.Execution.PermissionMode != "plan" {
		t.Fatalf("plan identity changed: %#v, %v", saved, err)
	}
}

func TestParentContinuationRejectsLegacyIdentityWithoutStartingProvider(t *testing.T) {
	dir := t.TempDir()
	writer, err := agent.NewSubAgentTranscript(dir, "agt-legacy-parent", agent.NewSubAgentHeader("agt-legacy-parent", "model", "parent", "worker", dir, "default"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &parentContinuationProvider{name: "provider"}
	tool := NewAgent(permission.New(permission.ModeDefault), provider, tools.NewRegistry(), "model", "system").
		WithRoster(agent.NewRoster(2)).WithSessionPersistence(dir, "parent")
	result, err := tool.Execute(context.Background(), map[string]any{"resume_from": "agt-legacy-parent", "prompt": "continue"})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "saved execution identity") || len(provider.captured()) != 0 {
		t.Fatalf("legacy identity silently guessed: %#v, %v", result, err)
	}
}

func TestParentContinuationLiveFollowupUsesSameDurableProducer(t *testing.T) {
	dir := t.TempDir()
	provider := &parentContinuationProvider{name: "provider", entered: make(chan struct{}), release: make(chan struct{})}
	roster := agent.NewRoster(2)
	t.Cleanup(roster.CancelAll)
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "model", "system").
		WithRoster(roster).WithSessionPersistence(dir, "parent")
	parentEvents := make(chan agent.Event, 64)
	ctx, cancel := context.WithTimeout(agent.WithEventOut(context.Background(), parentEvents), 3*time.Second)
	defer cancel()
	first, err := tool.Execute(ctx, map[string]any{"prompt": "original live task", "run_in_background": true})
	if err != nil || first == nil || first.IsError {
		t.Fatalf("start: %#v, %v", first, err)
	}
	id, _ := first.Meta["agent_id"].(string)
	if id == "" {
		t.Fatalf("missing background child ID: %#v", first)
	}
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("first child never started")
	}
	result, err := tool.Execute(ctx, map[string]any{"resume_from": id, "prompt": "parent follow-up"})
	if err != nil || result == nil || result.IsError || result.Meta["agent_id"] != id || result.Meta["continuation"] != true || result.Meta["input_id"] == "" {
		t.Fatalf("live continuation receipt: %#v, %v", result, err)
	}
	pending, err := agent.LoadSubAgentSnapshot(dir, id)
	if err != nil || len(pending.PendingFollowups) != 1 || pending.PendingFollowups[0].Text != "parent follow-up" || len(provider.captured()) != 1 {
		t.Fatalf("input was not durably accepted before acknowledgement: %#v, %v", pending, err)
	}
	close(provider.release)
	for {
		select {
		case event := <-parentEvents:
			if event.Kind != agent.EventSubAgentEnd {
				continue
			}
			if event.SubAgentID != id || event.SubAgentStatus != "completed" {
				t.Fatalf("unexpected terminal: %#v", event)
			}
			snapshot, loadErr := agent.LoadSubAgentSnapshot(dir, id)
			if loadErr != nil || len(snapshot.PendingFollowups) != 0 || len(provider.captured()) != 2 {
				t.Fatalf("follow-up did not complete on same child: %#v, %v, requests=%d", snapshot, loadErr, len(provider.captured()))
			}
			entries, readErr := os.ReadDir(filepath.Join(dir, agent.SubAgentTranscriptDirname))
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("duplicate child transcript: %#v %v", entries, readErr)
			}
			return
		case <-ctx.Done():
			t.Fatal("same child did not complete continued task")
		}
	}
}

func TestParentContinuationPermissionPolicyHashSurvivesSessionRestore(t *testing.T) {
	original := permission.New(permission.ModeDefault)
	original.AppendRules(permission.Rule{Tool: "Write", Match: "notes.md", Verb: permission.DecisionAllow, Source: "interactive"})
	restored := permission.New(permission.ModeDefault)
	restored.AppendRules(permission.Rule{Tool: "Write", Match: "notes.md", Verb: permission.DecisionAllow, Source: permission.ResumedSessionSource("interactive")})
	if agentPermissionRulesHash(original) != agentPermissionRulesHash(restored) {
		t.Fatal("diagnostic source relabeling changed the same permission policy")
	}
	restored.SetMode(permission.ModePlan)
	if agentPermissionRulesHash(original) != agentPermissionRulesHash(restored) {
		t.Fatal("mode is separately validated and must not change rule identity")
	}
	policy := permission.New(permission.ModeDefault)
	policy.AppendRules(permission.Rule{Tool: "Write", Match: "notes.md", Verb: permission.DecisionAllow, Source: "policy:allow"})
	config := permission.New(permission.ModeDefault)
	config.AppendRules(permission.Rule{Tool: "Write", Match: "notes.md", Verb: permission.DecisionAllow, Source: "config:allow"})
	if agentPermissionRulesHash(policy) == agentPermissionRulesHash(config) {
		t.Fatal("different rule authority was erased")
	}
	denyThenAllow := permission.New(permission.ModeDefault)
	denyThenAllow.AppendRules(permission.Rule{Tool: "Write", Verb: permission.DecisionDeny, Source: "interactive"}, permission.Rule{Tool: "Write", Verb: permission.DecisionAllow, Source: "interactive"})
	allowThenDeny := permission.New(permission.ModeDefault)
	allowThenDeny.AppendRules(permission.Rule{Tool: "Write", Verb: permission.DecisionAllow, Source: "interactive"}, permission.Rule{Tool: "Write", Verb: permission.DecisionDeny, Source: "interactive"})
	if agentPermissionRulesHash(denyThenAllow) == agentPermissionRulesHash(allowThenDeny) {
		t.Fatal("last-rule-wins policy ordering was erased")
	}
}
