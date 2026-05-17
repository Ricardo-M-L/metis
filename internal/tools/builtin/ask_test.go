package builtin

// AskUser tool — covers the three executable paths:
//   1. headless (no event channel) → structured error
//   2. normal (event surfaces; UI replies through channel)
//   3. user-dismissed (empty reply) → IsError
// plus the normalization edge cases (empty options stripped, >9
// options capped, empty question rejected).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestAskUser_HeadlessReturnsStructuredError(t *testing.T) {
	tool := AskUser{}
	res, err := tool.Execute(context.Background(), map[string]any{
		"question": "pick one",
		"options":  []any{"a", "b"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("headless path should return IsError=true; got %+v", res)
	}
	if !strings.Contains(res.Output, "no interactive UI") {
		t.Errorf("headless error should mention non-interactive context; got %q", res.Output)
	}
}

func TestAskUser_EmptyQuestionRejected(t *testing.T) {
	tool := AskUser{}
	res, _ := tool.Execute(context.Background(), map[string]any{
		"question": "   ",
	})
	if res == nil || !res.IsError {
		t.Fatalf("blank question should produce IsError=true; got %+v", res)
	}
	if !strings.Contains(res.Output, "question is required") {
		t.Errorf("expected 'question is required' error; got %q", res.Output)
	}
}

func TestAskUser_EventCarriesOptionsAndReply(t *testing.T) {
	tool := AskUser{}
	ch := make(chan agent.Event, 1)
	ctx := agent.WithEventOut(context.Background(), ch)

	doneCh := make(chan struct{})
	var res *struct {
		out     string
		isError bool
	}
	go func() {
		r, _ := tool.Execute(ctx, map[string]any{
			"question": "which DB?",
			"options":  []any{"postgres", "mysql", "sqlite", ""},
		})
		res = &struct {
			out     string
			isError bool
		}{r.Output, r.IsError}
		close(doneCh)
	}()

	select {
	case ev := <-ch:
		if ev.Kind != agent.EventAskUser {
			t.Fatalf("expected EventAskUser, got %v", ev.Kind)
		}
		if ev.AskUserQuestion != "which DB?" {
			t.Errorf("question not propagated; got %q", ev.AskUserQuestion)
		}
		// Empty option string was stripped during normalization.
		if len(ev.AskUserOptions) != 3 {
			t.Errorf("expected 3 normalized options (empty stripped); got %d", len(ev.AskUserOptions))
		}
		if ev.AskUserReply == nil {
			t.Fatalf("reply channel must be set")
		}
		ev.AskUserReply <- "postgres"
	case <-time.After(2 * time.Second):
		t.Fatalf("tool did not emit event in time")
	}

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("tool did not return after reply")
	}
	if res.isError {
		t.Errorf("user-picked answer should not be an error; got %+v", res)
	}
	if res.out != "postgres" {
		t.Errorf("returned answer mismatch; got %q", res.out)
	}
}

func TestAskUser_EmptyReplyIsDismissalError(t *testing.T) {
	tool := AskUser{}
	ch := make(chan agent.Event, 1)
	ctx := agent.WithEventOut(context.Background(), ch)

	doneCh := make(chan struct{})
	var isError bool
	var output string
	go func() {
		r, _ := tool.Execute(ctx, map[string]any{
			"question": "pick",
			"options":  []any{"x", "y"},
		})
		output = r.Output
		isError = r.IsError
		close(doneCh)
	}()

	ev := <-ch
	ev.AskUserReply <- ""
	<-doneCh

	if !isError {
		t.Errorf("empty reply should surface as IsError; got %q", output)
	}
	if !strings.Contains(output, "dismissed") {
		t.Errorf("expected 'dismissed' in output, got %q", output)
	}
}

func TestAskUser_NormalizationCapsOptionsAt9(t *testing.T) {
	tool := AskUser{}
	ch := make(chan agent.Event, 1)
	ctx := agent.WithEventOut(context.Background(), ch)

	// Build 15 numbered options — model went wild.
	opts := make([]any, 15)
	for i := range opts {
		opts[i] = "option-" + string(rune('a'+i))
	}

	doneCh := make(chan struct{})
	go func() {
		_, _ = tool.Execute(ctx, map[string]any{
			"question": "too many",
			"options":  opts,
		})
		close(doneCh)
	}()

	ev := <-ch
	if len(ev.AskUserOptions) != 9 {
		t.Errorf("options should cap at 9 (keyboard 1-9); got %d", len(ev.AskUserOptions))
	}
	ev.AskUserReply <- "option-a"
	<-doneCh
}

func TestAskUser_NoOptionsForcesFreeform(t *testing.T) {
	tool := AskUser{}
	ch := make(chan agent.Event, 1)
	ctx := agent.WithEventOut(context.Background(), ch)

	doneCh := make(chan struct{})
	go func() {
		_, _ = tool.Execute(ctx, map[string]any{
			"question":       "open-ended question",
			"allow_freeform": false,
		})
		close(doneCh)
	}()

	ev := <-ch
	if !ev.AskUserAllowFreeform {
		t.Errorf("missing options must auto-enable freeform so user has SOME answer surface")
	}
	ev.AskUserReply <- "typed"
	<-doneCh
}
