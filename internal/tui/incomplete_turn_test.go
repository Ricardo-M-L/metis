package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type incompleteTUIStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *incompleteTUIStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*incompleteTUIStream) Close() error { return nil }

type incompleteTUIProvider struct{}

func (incompleteTUIProvider) Name() string          { return "incomplete-tui" }
func (incompleteTUIProvider) ModelID() string       { return "incomplete-tui-model" }
func (incompleteTUIProvider) MaxContextTokens() int { return 100_000 }
func (incompleteTUIProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream only")
}
func (incompleteTUIProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &incompleteTUIStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "partial"},
		{Type: "message_delta", StopReason: "max_tokens"},
		{Type: "message_stop"},
	}}, nil
}

func TestRunTurnAsyncReportsIncompleteTerminal(t *testing.T) {
	loop := agent.NewLoop(incompleteTUIProvider{}, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.AppendUser("answer")
	turnCtx, cancel := context.WithCancel(context.Background())
	events := make(chan agent.Event, 16)
	done := make(chan error, 1)

	runTurnAsync(turnCtx, cancel, loop, "", events, done)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("done error = %v, want max_tokens incomplete", err)
	}
}
