package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestWebContextIsDurableBeforeProvider(t *testing.T) {
	for _, broken := range []bool{false, true} {
		name := "durable"
		if broken {
			name = "storage-failure"
		}
		t.Run(name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "context-checkpoint"
			if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "fixture", Model: "fixture", System: "base"}); err != nil {
				t.Fatal(err)
			}
			p := &webCheckpointProvider{activationTestProvider: activationTestProvider{name: "fixture", model: "fixture"}, store: store, id: id}
			loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "base", 8)
			loop.SystemSections = []llm.SystemSection{{Name: "base", Body: "base", Cache: true}, {Name: "env", Body: "current state", Volatile: true}}
			s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "fixture", InitialSessionID: id})
			var faultErr error
			if broken {
				path := filepath.Join(store.Dir, id+".jsonl")
				backup := path + ".fixture-backup"
				blocked := false
				loop.Hooks.Register(agent.TurnStartHandler(func(context.Context, agent.HookContext, int) {
					if blocked || faultErr != nil {
						return
					}
					if faultErr = os.Rename(path, backup); faultErr == nil {
						faultErr = os.Mkdir(path, 0700)
						blocked = faultErr == nil
					}
				}))
				t.Cleanup(func() {
					if blocked {
						if err := os.Remove(path); err != nil {
							t.Error(err)
						}
						if err := os.Rename(backup, path); err != nil {
							t.Error(err)
						}
					}
				})
			}
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"sessionId":"context-checkpoint","input":"persist state first"}`)))
			if faultErr != nil {
				t.Fatal(faultErr)
			}
			if broken {
				if rr.Code == http.StatusOK || p.calls != 0 {
					t.Fatalf("unwritable web session reached provider: status=%d calls=%d body=%s", rr.Code, p.calls, rr.Body.String())
				}
				return
			}
			if rr.Code != http.StatusOK || !p.durable {
				t.Fatalf("web context was not durable before request: status=%d durable=%v body=%s", rr.Code, p.durable, rr.Body.String())
			}
		})
	}
}

type webCheckpointProvider struct {
	activationTestProvider
	store   *session.Store
	id      string
	calls   int
	durable bool
}

func (*webCheckpointProvider) ContextUpdateScope() string { return "fixture-context" }
func (p *webCheckpointProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	_, history, err := p.store.Load(p.id)
	if err != nil {
		return nil, err
	}
	for _, message := range history {
		for _, block := range message.Content {
			p.durable = p.durable || block.Type == "context_update"
		}
	}
	return &composerSummaryStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "done"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}, nil
}
