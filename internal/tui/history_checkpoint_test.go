package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type checkpointTUIProvider struct {
	steps    int
	requests int
	before   func(int, llm.Request) error
}

func (*checkpointTUIProvider) Name() string          { return "checkpoint-tui" }
func (*checkpointTUIProvider) ModelID() string       { return "test-model" }
func (*checkpointTUIProvider) MaxContextTokens() int { return 200_000 }
func (*checkpointTUIProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("stream only")
}
func (p *checkpointTUIProvider) Stream(_ context.Context, request llm.Request) (llm.StreamReader, error) {
	p.requests++
	if p.before != nil {
		if err := p.before(p.requests, request); err != nil {
			return nil, err
		}
	}
	if p.requests > p.steps {
		return &incompleteTUIStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "Finished all numbered steps."},
			{Type: "message_delta", StopReason: "end_turn", OutputTokens: 10},
			{Type: "message_stop"},
		}}, nil
	}
	id := fmt.Sprintf("checkpoint-step-%d", p.requests)
	return &incompleteTUIStream{events: []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: id, ToolName: "CheckpointStep"},
		{Type: "tool_input_delta", ToolUseID: id, InputDelta: fmt.Sprintf(`{"step":%d}`, p.requests)},
		{Type: "tool_use_stop", ToolUseID: id},
		{Type: "message_delta", StopReason: "tool_use", OutputTokens: 10},
		{Type: "message_stop"},
	}}, nil
}

type checkpointTUIStep struct{ tools.BaseTool }

func (checkpointTUIStep) Name() string        { return "CheckpointStep" }
func (checkpointTUIStep) Description() string { return "perform one numbered step" }
func (checkpointTUIStep) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"step": map[string]any{"type": "integer"}}}
}
func (checkpointTUIStep) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (checkpointTUIStep) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (checkpointTUIStep) Execute(_ context.Context, input map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: fmt.Sprintf("completed numbered step %v: %s", input["step"], strings.Repeat("evidence ", 40))}, nil
}

func checkpointTUIResultCount(history []llm.Message) int {
	count := 0
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				count++
			}
		}
	}
	return count
}

type checkpointTUIFixture struct {
	model   *Model
	repl    *REPL
	store   *session.Store
	run     func() error
	persist func() error
}

func newCheckpointTUIFixture(t *testing.T, frontend string, provider *checkpointTUIProvider) checkpointTUIFixture {
	t.Helper()
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.Provider = provider
	m.loop.MaxIters = 100
	m.loop.ContinueOnIterationLimit = true
	m.loop.Registry.Register(checkpointTUIStep{})
	fixture := checkpointTUIFixture{model: m, store: store, persist: m.persistTail}
	if frontend == "repl" {
		r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
		if err != nil {
			t.Fatal(err)
		}
		r.out = &bytes.Buffer{}
		fixture.repl = r
		fixture.persist = r.persistTail
		fixture.run = func() error { return r.runTurn(context.Background()) }
	} else {
		fixture.run = func() error {
			ctx, cancel := context.WithCancel(context.Background())
			events := make(chan agent.Event, 8192)
			done := make(chan error, 1)
			// Deliberately leave the consumer idle: durability must be owned by
			// Run's producer, rather than a possibly delayed EventTurnEnd reader.
			runTurnAsync(ctx, cancel, m.loop, "source", events, done)
			return <-done
		}
	}
	return fixture
}

func TestInteractiveHistoryCheckpointPrecedesRequestsAcrossSegments(t *testing.T) {
	for _, frontend := range []string{"tui", "repl"} {
		t.Run(frontend, func(t *testing.T) {
			provider := &checkpointTUIProvider{steps: 205}
			fixture := newCheckpointTUIFixture(t, frontend, provider)
			provider.before = func(n int, request llm.Request) error {
				_, durable, err := fixture.store.Load("source")
				if err != nil {
					return err
				}
				if got := checkpointTUIResultCount(durable); got != n-1 || got != checkpointTUIResultCount(request.Messages) {
					return fmt.Errorf("request %d: disk has %d completed results, want %d", n, got, n-1)
				}
				return nil
			}
			fixture.model.loop.AppendUser("perform all 205 numbered steps, then report the result")
			if err := fixture.run(); err != nil {
				t.Fatal(err)
			}
			if provider.requests != 206 {
				t.Fatalf("provider requests = %d, want 206", provider.requests)
			}
			if err := fixture.persist(); err != nil {
				t.Fatal(err)
			}
			assertStoredTail(t, fixture.store, "source", fixture.model.loop.History())
		})
	}
}

func TestInteractiveHistoryCheckpointSharesCompactionAndFinalCursor(t *testing.T) {
	for _, frontend := range []string{"tui", "repl"} {
		t.Run(frontend, func(t *testing.T) {
			fixture := newCheckpointTUIFixture(t, frontend, &checkpointTUIProvider{})
			loop := fixture.model.loop
			if loop.HistoryCheckpoint == nil {
				t.Fatal("HistoryCheckpoint is not installed")
			}
			before := completedTailHistory()
			if err := loop.HistoryCheckpoint(before); err != nil {
				t.Fatal(err)
			}
			after := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "compacted conversation"}}}}
			if err := loop.CompactionCheckpoint(before, after); err != nil {
				t.Fatal(err)
			}
			final := append(after, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "final answer"}}})
			loop.Restore(final)
			if err := loop.HistoryCheckpoint(final); err != nil {
				t.Fatal(err)
			}
			if err := fixture.persist(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.persist(); err != nil {
				t.Fatal(err)
			}
			assertStoredTail(t, fixture.store, "source", final)
			ledger, err := os.ReadFile(filepath.Join(fixture.store.Dir, "source.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			messages, replacements := 0, 0
			for _, line := range bytes.Split(bytes.TrimSpace(ledger), []byte("\n")) {
				var entry session.Entry
				if err := json.Unmarshal(line, &entry); err != nil {
					t.Fatal(err)
				}
				if entry.Type == "message" {
					messages++
				} else if entry.Type == "history_replace" {
					replacements++
				}
			}
			if messages != len(before)+1 || replacements != 1 {
				t.Fatalf("ledger has %d messages and %d replacements, want %d and 1", messages, replacements, len(before)+1)
			}
		})
	}
}

func TestInteractiveHistoryCheckpointFailureStopsBeforeNextRequest(t *testing.T) {
	for _, frontend := range []string{"tui", "repl"} {
		t.Run(frontend, func(t *testing.T) {
			provider := &checkpointTUIProvider{steps: 5}
			fixture := newCheckpointTUIFixture(t, frontend, provider)
			provider.before = func(n int, _ llm.Request) error {
				if n == 2 {
					temporarilyBlockSessionTail(t, fixture.store, "source")
				}
				return nil
			}
			fixture.model.loop.AppendUser("complete the steps")
			err := fixture.run()
			if err == nil || provider.requests != 2 || checkpointTUIResultCount(fixture.model.loop.History()) != 2 {
				t.Fatalf("checkpoint failure: err=%v requests=%d completed results=%d", err, provider.requests, checkpointTUIResultCount(fixture.model.loop.History()))
			}
			formatted, hint := formatTurnError(err)
			if !strings.HasPrefix(formatted, "Session save error:") || hint == "" || strings.Contains(formatted, fixture.store.Dir) || strings.Contains(hint, fixture.store.Dir) {
				t.Fatalf("checkpoint failure mislabeled or private detail exposed: %q, %q", formatted, hint)
			}
			if fixture.repl != nil && strings.Contains(fixture.repl.out.(*bytes.Buffer).String(), fixture.store.Dir) {
				t.Fatal("REPL exposed private storage detail")
			}
			if fixture.repl == nil {
				fixture.model.finalizeTurn(err)
				for _, message := range fixture.model.messages {
					if strings.Contains(message.Content, fixture.store.Dir) || strings.Contains(message.Content, "API Error:") {
						t.Fatalf("TUI exposed or mislabeled storage failure: %q", message.Content)
					}
				}
			}
		})
	}
}

func TestInteractiveHistoryCheckpointFollowsIdleSessionSwitch(t *testing.T) {
	for _, frontend := range []string{"tui", "repl"} {
		t.Run(frontend, func(t *testing.T) {
			fixture := newCheckpointTUIFixture(t, frontend, &checkpointTUIProvider{})
			loop := fixture.model.loop
			if loop.HistoryCheckpoint == nil {
				t.Fatal("HistoryCheckpoint is not installed")
			}
			original := completedTailHistory()
			loop.Restore(original)
			if err := loop.HistoryCheckpoint(original); err != nil {
				t.Fatal(err)
			}
			target := "target"
			if fixture.repl != nil {
				var err error
				target, err = fixture.repl.startFreshSession()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				header := &session.Header{ID: target, Model: "test-model"}
				if err := fixture.store.WriteHeaderFull(*header); err != nil {
					t.Fatal(err)
				}
				if err := fixture.model.activateSession(target, header, nil, true); err != nil {
					t.Fatal(err)
				}
			}
			loop.AppendUser("new session prompt")
			want := loop.History()
			if reflect.DeepEqual(want, original) {
				t.Fatal("session switch did not replace live history")
			}
			if err := loop.HistoryCheckpoint(want); err != nil {
				t.Fatal(err)
			}
			if err := fixture.persist(); err != nil {
				t.Fatal(err)
			}
			assertStoredTail(t, fixture.store, "source", original)
			assertStoredTail(t, fixture.store, target, want)
		})
	}
}
