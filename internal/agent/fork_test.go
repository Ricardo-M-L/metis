package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// scriptedProvider returns a queue of pre-canned responses, advancing
// per Complete call. Mirrors how openclaude tests its forked agent —
// no need to spin up a real LLM, we just want to verify the loop
// shape (assistant → tools → loop → end_turn).
type scriptedProvider struct {
	mu    sync.Mutex
	resps []*llm.Response
	calls []llm.Request
}

func (p *scriptedProvider) Name() string          { return "scripted" }
func (p *scriptedProvider) MaxContextTokens() int { return 200_000 }
func (p *scriptedProvider) ModelID() string       { return "" }
func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if len(p.resps) == 0 {
		return &llm.Response{StopReason: "end_turn"}, nil
	}
	r := p.resps[0]
	p.resps = p.resps[1:]
	return r, nil
}
func (p *scriptedProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("scriptedProvider: streaming not implemented")
}

// forkFakeTool — Execute returns a fixed Output unless the input has
// "fail":true. Named distinctly from dispatch_test.go's fakeTool.
type forkFakeTool struct {
	name        string
	desc        string
	concurrency pubtool.Concurrency
}

func (t forkFakeTool) Name() string                { return t.name }
func (t forkFakeTool) Description() string         { return t.desc }
func (t forkFakeTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (t forkFakeTool) Concurrency(map[string]any) pubtool.Concurrency {
	if t.concurrency == 0 {
		return pubtool.ConcurrencySafe
	}
	return t.concurrency
}
func (t forkFakeTool) CanUse(context.Context, map[string]any) (pubtool.Permission, string) {
	return pubtool.PermissionAllow, ""
}
func (t forkFakeTool) Execute(_ context.Context, in map[string]any) (*pubtool.Result, error) {
	if v, ok := in["fail"].(bool); ok && v {
		return nil, errors.New("fake fail")
	}
	if v, ok := in["payload"].(string); ok {
		return &pubtool.Result{Output: v}, nil
	}
	return &pubtool.Result{Output: "ok"}, nil
}

func newRegistryWith(t *testing.T, ts ...tools.Tool) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry()
	for _, x := range ts {
		r.Register(x)
	}
	return r
}

func TestRunForkedAgent_EndTurnImmediate(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "all done"}}, StopReason: "end_turn",
				InputTokens: 100, OutputTokens: 5, CacheReadInputTokens: 80},
		},
	}
	reg := newRegistryWith(t)
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "extract memos",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   3,
		ForkLabel:  "test",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", res.StopReason)
	}
	if res.Usage.InputTokens != 100 || res.Usage.CacheReadInputTokens != 80 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	if got := res.LastAssistantText(); got != "all done" {
		t.Errorf("LastAssistantText = %q", got)
	}
}

func TestRunForkedAgent_OneToolThenEnd(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "u1", ToolName: "fake_read",
				ToolInput: map[string]any{"payload": "hello"},
			}}, StopReason: "tool_use"},
			{Content: []llm.ContentBlock{{Type: "text", Text: "got it"}}, StopReason: "end_turn"},
		},
	}
	reg := newRegistryWith(t, forkFakeTool{name: "fake_read", desc: "fake"})
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "go",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   5,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	// Verify the tool_result got fed back: messages should be
	// user(prompt) → assistant(tool_use) → user(tool_result) → assistant(text)
	if len(res.NewMessages) != 4 {
		t.Fatalf("NewMessages count = %d, want 4: %+v", len(res.NewMessages), res.NewMessages)
	}
	tr := res.NewMessages[2]
	if tr.Role != llm.RoleUser || len(tr.Content) != 1 || tr.Content[0].Type != "tool_result" {
		t.Errorf("expected tool_result user message at idx 2, got %+v", tr)
	}
	if tr.Content[0].ToolResult != "hello" {
		t.Errorf("ToolResult = %q, want hello", tr.Content[0].ToolResult)
	}
}

func TestRunForkedAgent_DenyByCanUseTool(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "u1", ToolName: "fake_write",
				ToolInput: map[string]any{"payload": "evil"},
			}}, StopReason: "tool_use"},
			{Content: []llm.ContentBlock{{Type: "text", Text: "abandoning"}}, StopReason: "end_turn"},
		},
	}
	reg := newRegistryWith(t, forkFakeTool{name: "fake_write", desc: "fake"})
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:  CacheSafeParams{Provider: prov, Model: "test"},
		Prompt: "go",
		CanUseTool: func(_ context.Context, name string, _ map[string]any) (bool, string) {
			if name == "fake_write" {
				return false, "no writes in fork"
			}
			return true, ""
		},
		Registry: reg,
		MaxTurns: 5,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := res.NewMessages[2]
	if !tr.Content[0].IsError {
		t.Errorf("expected IsError=true on denied tool: %+v", tr.Content[0])
	}
	if !strings.Contains(tr.Content[0].ToolResult, "no writes in fork") {
		t.Errorf("expected reason in result: %q", tr.Content[0].ToolResult)
	}
}

func TestRunForkedAgent_UnknownToolBecomesIsError(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "u1", ToolName: "no_such_tool",
				ToolInput: map[string]any{},
			}}, StopReason: "tool_use"},
			{Content: []llm.ContentBlock{{Type: "text", Text: "stop"}}, StopReason: "end_turn"},
		},
	}
	reg := newRegistryWith(t)
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "go",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   5,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := res.NewMessages[2]
	if !tr.Content[0].IsError {
		t.Errorf("expected IsError=true on unknown tool")
	}
	if !strings.Contains(tr.Content[0].ToolResult, "unknown tool") {
		t.Errorf("expected 'unknown tool' in result: %q", tr.Content[0].ToolResult)
	}
}

func TestRunForkedAgent_MaxTurnsCap(t *testing.T) {
	// Provider keeps returning tool_use forever — we should stop at maxTurns.
	resps := make([]*llm.Response, 10)
	for i := range resps {
		resps[i] = &llm.Response{
			Content: []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "u", ToolName: "fake_loop",
				ToolInput: map[string]any{},
			}},
			StopReason: "tool_use",
		}
	}
	prov := &scriptedProvider{resps: resps}
	reg := newRegistryWith(t, forkFakeTool{name: "fake_loop"})
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "infinite",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   3,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (max)", res.Turns)
	}
	if res.StopReason != "max_turns" {
		t.Errorf("StopReason = %q, want max_turns", res.StopReason)
	}
}

func TestRunForkedAgent_ProviderError(t *testing.T) {
	prov := &errProvider{err: errors.New("network blew up")}
	reg := newRegistryWith(t)
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "x",
		CanUseTool: AllowAll,
		Registry:   reg,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if res == nil || res.StopReason != "error" {
		t.Errorf("StopReason = %v", res)
	}
	if !strings.Contains(err.Error(), "network blew up") {
		t.Errorf("error = %v", err)
	}
}

type errProvider struct{ err error }

func (p *errProvider) Name() string          { return "err" }
func (p *errProvider) MaxContextTokens() int { return 1 }
func (p *errProvider) ModelID() string       { return "" }
func (p *errProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.err
}
func (p *errProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, p.err
}

func TestRunForkedAgent_ToolExecuteErrorBecomesIsError(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "u1", ToolName: "fake_fail",
				ToolInput: map[string]any{"fail": true},
			}}, StopReason: "tool_use"},
			{Content: []llm.ContentBlock{{Type: "text", Text: "bye"}}, StopReason: "end_turn"},
		},
	}
	reg := newRegistryWith(t, forkFakeTool{name: "fake_fail"})
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "go",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   5,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := res.NewMessages[2]
	if !tr.Content[0].IsError {
		t.Errorf("expected IsError=true on tool execute error")
	}
	if !strings.Contains(tr.Content[0].ToolResult, "execute error") {
		t.Errorf("got %q", tr.Content[0].ToolResult)
	}
}

func TestRunForkedAgent_CtxCancelled(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"}},
	}
	reg := newRegistryWith(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate
	_, err := RunForkedAgent(ctx, ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "go",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   5,
	})
	if err == nil {
		t.Fatalf("expected ctx cancel error")
	}
}

func TestRunForkedAgent_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		p    ForkedAgentParams
	}{
		{"empty prompt", ForkedAgentParams{
			Cache: CacheSafeParams{Provider: &scriptedProvider{}}, CanUseTool: AllowAll, Registry: newRegistryWith(t),
		}},
		{"nil canUseTool", ForkedAgentParams{
			Cache: CacheSafeParams{Provider: &scriptedProvider{}}, Prompt: "x", Registry: newRegistryWith(t),
		}},
		{"nil registry", ForkedAgentParams{
			Cache: CacheSafeParams{Provider: &scriptedProvider{}}, Prompt: "x", CanUseTool: AllowAll,
		}},
		{"nil provider", ForkedAgentParams{
			Cache: CacheSafeParams{}, Prompt: "x", CanUseTool: AllowAll, Registry: newRegistryWith(t),
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RunForkedAgent(context.Background(), tt.p); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestSnapshotForFork_HappyPath(t *testing.T) {
	prov := &scriptedProvider{}
	reg := newRegistryWith(t, forkFakeTool{name: "x"})
	loop := NewLoop(prov, reg, nil, nil, "you are helpful", 10)
	loop.Model = "claude-test"
	loop.AppendUser("hello")
	snap := SnapshotForFork(loop)
	if snap == nil {
		t.Fatalf("nil snapshot")
	}
	if snap.System != "you are helpful" {
		t.Errorf("System = %q", snap.System)
	}
	if snap.Model != "claude-test" {
		t.Errorf("Model = %q", snap.Model)
	}
	if len(snap.PrefixMessages) == 0 {
		t.Errorf("PrefixMessages should include the user message")
	}
}

func TestSnapshotForFork_NilLoop(t *testing.T) {
	if SnapshotForFork(nil) != nil {
		t.Fatalf("nil loop should yield nil snapshot")
	}
}

func TestForkInflight_Counter(t *testing.T) {
	before := ForkInflight()
	prov := &scriptedProvider{
		resps: []*llm.Response{{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"}},
	}
	reg := newRegistryWith(t)
	_, err := RunForkedAgentInstrumented(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "go",
		CanUseTool: AllowAll,
		Registry:   reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	after := ForkInflight()
	if before != after {
		t.Errorf("inflight should rebalance: before=%d after=%d", before, after)
	}
}

func (forkFakeTool) IsEnabled() bool { return true }
