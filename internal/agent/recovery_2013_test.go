package agent

// recovery_2013_test.go — locks the MiniMax "(2013) request entity too
// large" recovery path. Before this fix, tryRecoverOverflow string-
// matched on "context"/"too many tokens"/"exceeds limit" — none of
// which appear in MiniMax's user-facing error format. The auto-retry
// silently no-op'd and the user was stuck (image #9 user report
// 2026-05-10).
//
// The fix routes through ClassifyError so the same patterns the rest
// of the agent uses for 2013 detection take effect here.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestTryRecoverOverflow_RecognizesMiniMaxParen2013(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	l := &Loop{
		Compactor: c,
		Messages:  longMessages(20, 100), // 20 messages, well above ProtectFirst+Last+2
	}
	out := make(chan Event, 16)

	// Simulated MiniMax error from image #9.
	err := errors.New("provider rejected the request: anthropic 400: invalid params, request entity too large (2013) (invalid_request_error)")

	if !l.tryRecoverOverflow(context.Background(), err, out) {
		t.Fatalf("tryRecoverOverflow should fire for MiniMax (2013) error; it didn't")
	}
	close(out)
	// Recovery should have produced at least one info event.
	var info string
	for ev := range out {
		if ev.Kind == EventInfo {
			info += ev.Info + "\n"
		}
	}
	if !strings.Contains(info, "context overflow") {
		t.Errorf("expected 'context overflow' notice in events; got:\n%s", info)
	}
}

func TestTryRecoverOverflow_IgnoresUnrelatedErrors(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	l := &Loop{
		Compactor: c,
		Messages:  longMessages(20, 100),
	}
	out := make(chan Event, 4)
	// 401 unauthorized is NOT a context overflow; recovery shouldn't fire.
	err := errors.New("HTTP 401 unauthorized: token expired")
	if l.tryRecoverOverflow(context.Background(), err, out) {
		t.Errorf("tryRecoverOverflow fired for unrelated auth error")
	}
}

func TestTryRecoverOverflow_SnipsTailFirst(t *testing.T) {
	// When the conversation has a giant tool_result in the protected
	// tail (the typical "5MB grep dump as last turn" case), SnipAll
	// should rescue it without invoking the LLM-summarizer Compact.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	long := strings.Repeat("X", 10000)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", long), // would be in the tail (ProtectLast=3 → last 3 protected)
		toolUseMsg("t2", "Bash"),
		toolResultMsg("t2", long),
		msg(llm.RoleAssistant, "answer"),
	}
	l := &Loop{Compactor: c, Messages: msgs}
	out := make(chan Event, 8)

	err := errors.New("invalid params, request entity too large (2013)")
	if !l.tryRecoverOverflow(context.Background(), err, out) {
		t.Fatalf("recovery didn't fire on tail-snippable overflow")
	}
	close(out)
	// No summarizer call expected — SnipAll should have rescued first.
	if p.calls > 0 {
		t.Errorf("SnipAll should have rescued without invoking summarizer; got %d Stream calls", p.calls)
	}
	// Both tail tool_results should now be capped.
	for i, m := range l.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && len(b.ToolResult) >= 5000 {
				t.Errorf("msg[%d] tool_result not snipped after recovery; len=%d", i, len(b.ToolResult))
			}
		}
	}
}

// longMessages builds n alternating user/assistant messages each with
// `chars` of body. Used to satisfy tryRecoverOverflow's
// ProtectFirst+ProtectLast+2 size precondition.
func longMessages(n, chars int) []llm.Message {
	body := strings.Repeat("a", chars)
	out := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		out = append(out, msg(role, body))
	}
	return out
}
