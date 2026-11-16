package tui

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestInteractiveContextIsDurableBeforeProvider(t *testing.T) {
	for _, surface := range []string{"tui", "repl"} {
		for _, broken := range []bool{false, true} {
			name := surface + "/durable"
			if broken {
				name = surface + "/storage-failure"
			}
			t.Run(name, func(t *testing.T) {
				store, err := session.NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				const id = "context-checkpoint"
				if err := store.WriteHeader(id, "fixture", "base"); err != nil {
					t.Fatal(err)
				}
				p := &interactiveCheckpointProvider{store: store, id: id}
				gate := permission.New(permission.ModeDefault)
				loop := agent.NewLoop(p, tools.NewRegistry(), gate, nil, "base", 8)
				loop.SystemSections = []llm.SystemSection{
					{Name: "base", Body: "base", Cache: true},
					{Name: "state", Body: "current context", Volatile: true},
				}
				loop.AppendUser("persist context before sending")
				if err := store.AppendMessage(id, loop.History()[0]); err != nil {
					t.Fatal(err)
				}
				var persist func() error
				if surface == "tui" {
					m := NewModel(context.Background(), loop, nil, nil, store, id, gate, "fixture", "", "", &config.Config{})
					persist = m.persistTail
				} else {
					r, err := NewREPL(loop, nil, store, id, false, false, gate, "fixture", "")
					if err != nil {
						t.Fatal(err)
					}
					persist = r.persistTail
				}
				if broken {
					temporarilyBlockSessionTail(t, store, id)
				}
				err = loop.Run(context.Background(), make(chan agent.Event, 128))
				if broken {
					if err == nil || p.calls != 0 {
						t.Fatalf("unwritable session reached provider: calls=%d err=%v", p.calls, err)
					}
					return
				}
				if err != nil || !p.sawDurableContext {
					t.Fatalf("provider ran before context reached storage: durable=%v err=%v", p.sawDurableContext, err)
				}
				if err := persist(); err != nil {
					t.Fatal(err)
				}
				_, saved, err := store.Load(id)
				if err != nil || len(saved) != len(loop.History()) {
					t.Fatalf("surface tail duplicated producer checkpoints: saved=%d history=%d err=%v", len(saved), len(loop.History()), err)
				}
			})
		}
	}
}

type interactiveCheckpointProvider struct {
	store             *session.Store
	id                string
	calls             int
	sawDurableContext bool
}

func (*interactiveCheckpointProvider) Name() string               { return "fixture" }
func (*interactiveCheckpointProvider) ModelID() string            { return "fixture" }
func (*interactiveCheckpointProvider) MaxContextTokens() int      { return 128000 }
func (*interactiveCheckpointProvider) ContextUpdateScope() string { return "fixture-context" }
func (*interactiveCheckpointProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream-only fixture")
}
func (p *interactiveCheckpointProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	_, saved, err := p.store.Load(p.id)
	if err != nil {
		return nil, err
	}
	for _, message := range saved {
		for _, block := range message.Content {
			p.sawDurableContext = p.sawDurableContext || block.Type == "context_update"
		}
	}
	return &interactiveCheckpointStream{}, nil
}

type interactiveCheckpointStream struct{ step int }

func (s *interactiveCheckpointStream) Recv() (llm.StreamEvent, error) {
	events := []llm.StreamEvent{{Type: "text_delta", TextDelta: "done"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}
	if s.step == len(events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := events[s.step]
	s.step++
	return event, nil
}
func (*interactiveCheckpointStream) Close() error { return nil }
