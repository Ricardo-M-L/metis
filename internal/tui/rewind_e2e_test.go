package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

type rewindSummaryProvider struct{ calls atomic.Int32 }

func (p *rewindSummaryProvider) Name() string          { return "rewind-summary-test" }
func (p *rewindSummaryProvider) ModelID() string       { return "rewind-summary-test" }
func (p *rewindSummaryProvider) MaxContextTokens() int { return 200_000 }
func (p *rewindSummaryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "ASYNC_SUMMARY"}}}, nil
}
func (p *rewindSummaryProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls.Add(1)
	return &rewindSummaryStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "ASYNC_SUMMARY"},
		{Type: "message_stop"},
	}}, nil
}

type rewindSummaryStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *rewindSummaryStream) Close() error { return nil }
func (s *rewindSummaryStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func rewindSummaryPicker() *screen.RewindScreen {
	picker := screen.NewRewindScreen([]screen.RewindEntry{{Turn: 2, Prompt: "second prompt"}})
	picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for range 3 {
		picker.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return picker
}

func configureRewindSummary(t *testing.T, m *Model) *rewindSummaryProvider {
	t.Helper()
	provider := &rewindSummaryProvider{}
	m.loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), "rewind-summary-test", 200_000, provider)
	return provider
}

func rewindHistory() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first prompt"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "first answer"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "second prompt"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "second answer"}}},
	}
}

func TestRewindSlashOpensMessagePickerAndRestoresConversationOnly(t *testing.T) {
	m := newSlashTestModel(t)
	history := rewindHistory()
	m.loop.Restore(history)
	m.historyCursor = session.NewHistoryCursor(history)
	m.messages = []Message{
		{Role: "user", Content: "first prompt", Timestamp: time.Now()},
		{Role: "assistant", Content: "first answer", Timestamp: time.Now()},
		{Role: "user", Content: "second prompt", Timestamp: time.Now()},
		{Role: "assistant", Content: "second answer", Timestamp: time.Now()},
	}
	m.input.SetValue("/rewind")
	pressEnter(t, m)

	if _, ok := m.activeScreen.(*screen.RewindScreen); !ok {
		t.Fatalf("/rewind activeScreen=%T, want *screen.RewindScreen", m.activeScreen)
	}
	// Newest prompt (turn 2) is selected. Enter opens the action page;
	// Down chooses conversation-only; Enter applies it through Model.Update.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.activeScreen != nil {
		t.Fatalf("completed rewind screen remains active: %T", m.activeScreen)
	}
	if got := m.loop.CountTurns(); got != 1 {
		t.Fatalf("conversation rewind kept %d turns, want 1", got)
	}
	if got := m.input.Value(); got != "second prompt" {
		t.Fatalf("composer prefill=%q, want selected prompt", got)
	}
}

func TestRewindSummaryApplyReturnsCommandWithoutCallingProvider(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Restore(rewindHistory())
	provider := configureRewindSummary(t, m)

	cmd := m.applyScreenResult(rewindSummaryPicker())
	if cmd == nil {
		t.Fatal("summary apply returned nil command; provider work would block Update")
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider called synchronously %d time(s)", got)
	}
	if !m.rewindSummaryPending {
		t.Fatal("summary request did not enter in-progress state")
	}
	if got := m.loop.CountTurns(); got != 2 {
		t.Fatalf("history changed before async result: turns=%d", got)
	}
}

func TestRewindSummaryResultAppliesOnUpdate(t *testing.T) {
	m := newSlashTestModel(t)
	history := rewindHistory()
	m.loop.Restore(history)
	m.historyCursor = session.NewHistoryCursor(history)
	m.messages = []Message{
		{Role: "user", Content: "first prompt", Timestamp: time.Now()},
		{Role: "assistant", Content: "first answer", Timestamp: time.Now()},
		{Role: "user", Content: "second prompt", Timestamp: time.Now()},
		{Role: "assistant", Content: "second answer", Timestamp: time.Now()},
	}
	provider := configureRewindSummary(t, m)

	cmd := m.applyScreenResult(rewindSummaryPicker())
	msg := cmd()
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("async command provider calls=%d, want 1", got)
	}
	if got := m.loop.CountTurns(); got != 2 {
		t.Fatalf("command goroutine mutated history before Update: turns=%d", got)
	}
	m.Update(msg)

	if m.rewindSummaryPending {
		t.Fatal("completed result left in-progress guard set")
	}
	if got := m.input.Value(); got != "second prompt" {
		t.Fatalf("summary prompt prefill=%q", got)
	}
	var joined strings.Builder
	for _, message := range m.loop.History() {
		for _, block := range message.Content {
			joined.WriteString(block.Text)
		}
	}
	if !strings.Contains(joined.String(), "ASYNC_SUMMARY") || strings.Contains(joined.String(), "second answer") {
		t.Fatalf("summary result not applied correctly: %q", joined.String())
	}
}

func TestRewindSummaryDuplicateTriggerIsIgnoredWhilePending(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Restore(rewindHistory())
	provider := configureRewindSummary(t, m)

	first := m.applyScreenResult(rewindSummaryPicker())
	second := m.applyScreenResult(rewindSummaryPicker())
	if first == nil {
		t.Fatal("first summary did not return command")
	}
	if second != nil {
		t.Fatal("duplicate summary returned a second command")
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider called before first command ran: %d", got)
	}
	m.Update(first())
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("duplicate summary caused %d provider calls, want 1", got)
	}
}

func TestRewindSummaryConcurrentConversationChangeFailsCAS(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Restore(rewindHistory())
	configureRewindSummary(t, m)
	cmd := m.applyScreenResult(rewindSummaryPicker())
	m.loop.AppendUser("concurrent prompt")
	m.Update(cmd())

	var joined strings.Builder
	for _, message := range m.loop.History() {
		for _, block := range message.Content {
			joined.WriteString(block.Text)
		}
	}
	if !strings.Contains(joined.String(), "concurrent prompt") {
		t.Fatalf("CAS failure lost concurrent prompt: %q", joined.String())
	}
	if strings.Contains(joined.String(), "ASYNC_SUMMARY") {
		t.Fatalf("stale async result overwrote conversation: %q", joined.String())
	}
}

func TestRewindConversationPersistFailureLeavesLoopUnchanged(t *testing.T) {
	m := newSlashTestModel(t)
	history := rewindHistory()
	m.loop.Restore(history)
	m.historyCursor = session.NewHistoryCursor(history)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.session = store
	m.sessionID = "persist-failure"
	// A directory at the JSONL path makes append fail deterministically.
	if err := os.Mkdir(filepath.Join(m.session.Dir, m.sessionID+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	picker := screen.NewRewindScreen([]screen.RewindEntry{{Turn: 2, Prompt: "second prompt"}})
	picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	picker.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(picker)

	if got := m.loop.CountTurns(); got != 2 {
		t.Fatalf("failed persistence changed loop turns=%d", got)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("failed persistence prefilling composer=%q", got)
	}
}
