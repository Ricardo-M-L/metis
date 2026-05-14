package builtin

// agent_g3_test.go — locks Phase G.3 (named teammates + MessageTeammate,
// 2026-05-12). Pairs with peer_notify.go (recipient-side drain) and
// Roster.Lookup (sender-side resolution).
//
// Eight contracts pinned:
//
//   1. Valid `name` registers in the Roster keyed by that name.
//   2. Invalid `name` (bad chars / too long / starts with non-letter)
//      → IsError, no roster entry created.
//   3. Duplicate `name` while the first holder is still live →
//      IsError naming the conflict.
//   4. Anonymous (no `name`) still works as before (G.0 path) →
//      auto-named `_anon-<hex>`, Anonymous=true.
//   5. MessageTeammate delivers a body to the target's Mailbox; the
//      target sees a buffered message it can drain.
//   6. MessageTeammate to a missing name → IsError naming the
//      missing recipient.
//   7. MessageTeammate by agent_id also resolves (fallback path).
//   8. MessageTeammate with empty `to` / empty `body` → IsError
//      with clear field-required messages.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// waitForTeammate polls Roster.Lookup until the named teammate
// registers (background spawns register on a goroutine; the test
// would otherwise race) or the deadline elapses. Returns false
// when the timeout fires so the caller can produce a useful
// assertion message instead of an obscure "Lookup returned !ok".
func waitForTeammate(roster *agent.Roster, name string, d time.Duration) (*agent.Teammate, bool) {
	deadline := time.Now().Add(d)
	for {
		if tm, ok := roster.Lookup(name); ok {
			return tm, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockingProvider returns an llm.Provider whose Stream() never sends
// a message_stop event — the sub-loop sees only a text_delta and then
// the stream stays open until ctx is cancelled. Used by background-
// spawn tests that need to observe the teammate's Roster entry mid-
// flight: the default helloProvider finishes in microseconds, so the
// background goroutine Unregisters before Lookup runs and the test
// becomes racy.
//
// The blocking happens inside Recv(): it returns the first event,
// then blocks on ctx.Done() forever (or until Roster.CancelAll fires).
type blockingFakeProvider struct{}

func (blockingFakeProvider) Name() string          { return "blocking-fake" }
func (blockingFakeProvider) MaxContextTokens() int { return 100_000 }
func (blockingFakeProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (blockingFakeProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	return &blockingStream{ctx: ctx}, nil
}

type blockingStream struct {
	ctx  context.Context
	sent bool
}

func (s *blockingStream) Recv() (llm.StreamEvent, error) {
	if !s.sent {
		s.sent = true
		return llm.StreamEvent{Type: "text_delta", TextDelta: "live"}, nil
	}
	<-s.ctx.Done()
	return llm.StreamEvent{}, s.ctx.Err()
}
func (s *blockingStream) Close() error { return nil }

// TestAgentExecute_NamedTeammateRegisters — happy path: pass `name`,
// teammate appears in Roster under that name, Anonymous=false.
func TestAgentExecute_NamedTeammateRegisters(t *testing.T) {
	roster := agent.NewRoster(0)
	// blockingFakeProvider keeps the sub-loop alive (Recv blocks
	// after the first text_delta) so the background goroutine can't
	// Unregister before Lookup runs. The default helloProvider
	// finishes in microseconds — Execute returns, the sub-loop
	// goroutine completes, defer Unregister fires, and Lookup races
	// to see an empty Roster. With a blocking provider the teammate
	// stays registered until roster.CancelAll fires in the cleanup.
	tool := NewAgent(permission.New(permission.ModeBypass), blockingFakeProvider{}, tools.NewRegistry(), "model", "system").
		WithRoster(roster)

	// Use run_in_background so the registration stays observable
	// (foreground would Unregister before we can check Roster.Lookup).
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "x",
		"name":              "alice",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Fatalf("named spawn should succeed; got IsError: %s", res.Output)
	}

	// Background spawn registers asynchronously — Execute returns as
	// soon as the goroutine is launched, before Roster.Register has
	// run. Poll up to 2s so the test isn't racy under load.
	tm, ok := waitForTeammate(roster, "alice", 2*time.Second)
	if !ok {
		t.Fatalf("Lookup(\"alice\") should find the teammate within 2s")
	}
	if tm.Anonymous {
		t.Errorf("named teammate must have Anonymous=false; got true")
	}
	if !tm.Background {
		t.Errorf("background teammate must have Background=true")
	}

	// Cleanup
	roster.CancelAll()
}

// TestAgentExecute_NameValidation — various invalid name shapes
// reject with actionable error messages. The model needs to know
// which rule it tripped.
func TestAgentExecute_NameValidation(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	cases := []struct {
		name    string
		wantSub string
	}{
		{"1starts-with-digit", "must start with a letter"},
		{"_underscore-prefix", "must start with a letter"},
		{"-dash-prefix", "must start with a letter"},
		{"has space", "invalid char"},
		{"has@symbol", "invalid char"},
		{strings.Repeat("a", 33), "exceeds 32 chars"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := tool.Execute(context.Background(), map[string]any{
				"prompt": "x",
				"name":   c.name,
			})
			if err != nil {
				t.Fatalf("Execute err: %v", err)
			}
			if !res.IsError {
				t.Errorf("invalid name %q must be IsError; got %+v", c.name, res)
			}
			if !strings.Contains(res.Output, c.wantSub) {
				t.Errorf("name %q error should contain %q; got %q", c.name, c.wantSub, res.Output)
			}
		})
	}
}

// TestAgentExecute_DuplicateNameAutoSuffixed — same name twice while
// the first is live auto-suffixes to `<base>-2` since 2026-05-14
// (mirrors claude-code spawnMultiAgent.ts:267). Strict rejection
// stays available via Teammate.StrictName for callers like
// /agents resume that need exact-match semantics.
func TestAgentExecute_DuplicateNameAutoSuffixed(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), slowProvider(2_000_000_000), tools.NewRegistry(), "model", "system").
		WithRoster(roster)

	// First spawn occupies the name (background so it stays live).
	_, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "x",
		"name":              "carol",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("first spawn err: %v", err)
	}

	// Second spawn with same name auto-suffixes — no error, no IsError.
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "x",
		"name":              "carol",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("second spawn err: %v", err)
	}
	if res.IsError {
		t.Fatalf("duplicate name should auto-suffix, not error; got %+v", res)
	}
	// The roster should now have BOTH carol and carol-2 alive.
	if _, ok := roster.Lookup("carol"); !ok {
		t.Errorf("carol should still be registered")
	}
	if _, ok := roster.Lookup("carol-2"); !ok {
		t.Errorf("carol-2 should have been auto-assigned and registered")
	}
	roster.CancelAll()
}

// TestAgentExecute_AnonymousFallbackPath — no `name` field provided
// keeps the G.0/G.1 anonymous path working (`_anon-<hex>` auto-name,
// Anonymous=true). Backward-compat guard.
func TestAgentExecute_AnonymousFallbackPath(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), slowProvider(2_000_000_000), tools.NewRegistry(), "model", "system").
		WithRoster(roster)

	_, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "x",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("anon spawn err: %v", err)
	}
	teammates := roster.List()
	if len(teammates) != 1 {
		t.Fatalf("Roster should hold 1 teammate; got %d", len(teammates))
	}
	if !teammates[0].Anonymous {
		t.Errorf("no `name` arg must give Anonymous=true; got false (name=%q)", teammates[0].Name)
	}
	if !strings.HasPrefix(teammates[0].Name, "_anon-") {
		t.Errorf("anon name should start with _anon-; got %q", teammates[0].Name)
	}
	roster.CancelAll()
}

// TestMessageTeammate_DeliversToMailbox — sender pushes a body, the
// target's Mailbox receives it. Verifies the From/To/Sent metadata
// is intact.
func TestMessageTeammate_DeliversToMailbox(t *testing.T) {
	roster := agent.NewRoster(0)
	// Manually register a teammate (skip Agent tool for isolation).
	target := &agent.Teammate{Name: "bob"}
	if err := roster.Register(target); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tool := NewMessageTeammate(permission.New(permission.ModeBypass), roster)
	res, err := tool.Execute(context.Background(), map[string]any{
		"to":   "bob",
		"body": "hey bob, status update?",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Fatalf("delivery should succeed; got IsError: %s", res.Output)
	}
	if !strings.Contains(res.Output, "delivered to bob") {
		t.Errorf("result should confirm delivery; got %q", res.Output)
	}

	// Drain the mailbox — exactly one message present.
	select {
	case msg := <-target.Mailbox:
		if msg.Body != "hey bob, status update?" {
			t.Errorf("body roundtrip: got %q", msg.Body)
		}
		if msg.From != "main" {
			t.Errorf("From should be 'main' for root-loop sender; got %q", msg.From)
		}
		if msg.Sent.IsZero() {
			t.Errorf("Sent timestamp must be set")
		}
	default:
		t.Errorf("Mailbox should have a message ready to drain")
	}
}

// TestMessageTeammate_UnknownRecipient — missing name → IsError
// naming the recipient so the model can correlate to a SubAgentList.
func TestMessageTeammate_UnknownRecipient(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewMessageTeammate(permission.New(permission.ModeBypass), roster)
	res, err := tool.Execute(context.Background(), map[string]any{
		"to":   "ghost",
		"body": "anyone there?",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("unknown recipient must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "no teammate with name=ghost") {
		t.Errorf("error should echo the missing name; got %q", res.Output)
	}
}

// TestMessageTeammate_ByAgentID — `to` can be the agent_id as a
// fallback when the model only knows that handle from SubAgentList.
func TestMessageTeammate_ByAgentID(t *testing.T) {
	roster := agent.NewRoster(0)
	target := &agent.Teammate{Name: "dave", AgentID: "agt-abcd1234"}
	if err := roster.Register(target); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tool := NewMessageTeammate(permission.New(permission.ModeBypass), roster)
	res, err := tool.Execute(context.Background(), map[string]any{
		"to":   "agt-abcd1234",
		"body": "by id",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Errorf("by-id resolution should succeed; got %+v", res)
	}
	// Mailbox should have the message.
	select {
	case msg := <-target.Mailbox:
		if msg.Body != "by id" {
			t.Errorf("body got %q", msg.Body)
		}
	default:
		t.Errorf("Mailbox should have 1 message after by-id delivery")
	}
}

// TestMessageTeammate_RequiredFields — empty `to` / empty `body` both
// reject with field-specific messages.
func TestMessageTeammate_RequiredFields(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewMessageTeammate(permission.New(permission.ModeBypass), roster)

	cases := []struct {
		in      map[string]any
		wantSub string
	}{
		{map[string]any{"to": "", "body": "x"}, "`to`"},
		{map[string]any{"to": "anybody", "body": ""}, "`body`"},
		{map[string]any{"to": "   ", "body": "x"}, "`to`"},
	}
	for i, c := range cases {
		res, err := tool.Execute(context.Background(), c.in)
		if err != nil {
			t.Fatalf("case %d err: %v", i, err)
		}
		if !res.IsError {
			t.Errorf("case %d should be IsError; got %+v", i, res)
		}
		if !strings.Contains(res.Output, c.wantSub) {
			t.Errorf("case %d should mention %s; got %q", i, c.wantSub, res.Output)
		}
	}
}
