package agent

// fork_overflow_test.go — locks the MiniMax 2013 recovery path inside
// RunForkedAgent. Pre-fix: a fork inheriting the parent's full
// PrefixMessages would routinely exceed MiniMax's request body cap
// and bubble the 400 up as `forkedAgent: provider: anthropic 400`,
// breaking auto-memory extraction for any user past their first 30
// turns. Post-fix: the fork classifies the error and retries once
// with a snipped message slice.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// flakyOverflowProvider returns the configured overflow error on the
// FIRST Complete call, then a clean end_turn on the second. Lets us
// verify the fork actually retried.
type flakyOverflowProvider struct {
	mu        sync.Mutex
	calls     int
	overflow  error
	postRetry *llm.Response
	calls2    []llm.Request
}

func (p *flakyOverflowProvider) Name() string          { return "flaky" }
func (p *flakyOverflowProvider) MaxContextTokens() int { return 200_000 }
func (p *flakyOverflowProvider) ModelID() string       { return "" }
func (p *flakyOverflowProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.calls2 = append(p.calls2, req)
	if p.calls == 1 {
		return nil, p.overflow
	}
	return p.postRetry, nil
}
func (p *flakyOverflowProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("flaky: streaming not implemented")
}

func TestRunForkedAgent_RecoversFromMiniMax2013(t *testing.T) {
	// Prefix has a giant tool_result the fork inherits — exactly the
	// shape that triggers MiniMax's request-body cap.
	huge := strings.Repeat("X", 30_000)
	prefix := []llm.Message{
		msg(llm.RoleUser, "earlier"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", huge),
	}
	prov := &flakyOverflowProvider{
		overflow:  errors.New("anthropic 400: invalid params, request entity too large (2013) (invalid_request_error)"),
		postRetry: &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "fork done"}}, StopReason: "end_turn"},
	}
	reg := newRegistryWith(t)
	res, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache: CacheSafeParams{
			Provider:       prov,
			Model:          "MiniMax-M2.7",
			PrefixMessages: prefix,
		},
		Prompt:     "summarize",
		CanUseTool: AllowAll,
		Registry:   reg,
		MaxTurns:   3,
	})
	if err != nil {
		t.Fatalf("expected fork to recover, got error: %v", err)
	}
	if prov.calls != 2 {
		t.Errorf("expected 2 Complete calls (1 fail + 1 retry), got %d", prov.calls)
	}
	// The retry call's tool_result should be capped.
	retried := prov.calls2[1]
	for _, m := range retried.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && len(b.ToolResult) >= 30_000 {
				t.Errorf("retry's tool_result not snipped: len=%d", len(b.ToolResult))
			}
		}
	}
	if res.LastAssistantText() != "fork done" {
		t.Errorf("unexpected fork output: %q", res.LastAssistantText())
	}
}

func TestRunForkedAgent_NonOverflowErrorBubblesUp(t *testing.T) {
	// An auth error (not classified as overflow) must NOT trigger the
	// retry loop — recovery only makes sense for overflow.
	prov := &flakyOverflowProvider{
		overflow: errors.New("HTTP 401 unauthorized: token expired"),
	}
	reg := newRegistryWith(t)
	_, err := RunForkedAgent(context.Background(), ForkedAgentParams{
		Cache:      CacheSafeParams{Provider: prov, Model: "test"},
		Prompt:     "anything",
		CanUseTool: AllowAll,
		Registry:   reg,
	})
	if err == nil {
		t.Fatal("expected fork to bubble auth error, got nil")
	}
	if prov.calls != 1 {
		t.Errorf("expected exactly 1 Complete call (no retry on non-overflow), got %d", prov.calls)
	}
}

func TestSnipForkMessages_NoOpWhenAllUnderCap(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "small"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", strings.Repeat("a", 100)), // well under 5k cap
	}
	out := snipForkMessages(msgs)
	// No mutation expected — should return the same backing slice.
	if !sameSlice(out, msgs) {
		t.Errorf("snipForkMessages should return input slice when no clamping needed")
	}
}

func TestSnipForkMessages_ClampsOversized(t *testing.T) {
	huge := strings.Repeat("z", 20_000)
	msgs := []llm.Message{
		msg(llm.RoleUser, "ctx"),
		toolUseMsg("t1", "Grep"),
		toolResultMsg("t1", huge),
	}
	out := snipForkMessages(msgs)
	if sameSlice(out, msgs) {
		t.Fatalf("snipForkMessages should have produced a new slice")
	}
	got := out[2].Content[0].ToolResult
	if len(got) > PostCompactMaxToolResultChars+200 {
		t.Errorf("tool_result not clamped: len=%d", len(got))
	}
	if !strings.Contains(got, "[truncated post-compact:") {
		t.Errorf("missing clamp marker: %q", got[:120])
	}
}
