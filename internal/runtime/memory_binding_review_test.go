package runtime

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type reviewBindingProvider struct{ requests []llm.Request }

func (*reviewBindingProvider) Name() string          { return "review-offline" }
func (*reviewBindingProvider) ModelID() string       { return "review-offline" }
func (*reviewBindingProvider) MaxContextTokens() int { return 200000 }
func (p *reviewBindingProvider) Complete(_ context.Context, r llm.Request) (*llm.Response, error) {
	p.requests = append(p.requests, r)
	return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}
func (p *reviewBindingProvider) Stream(_ context.Context, r llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, r)
	return &reviewBindingStream{}, nil
}

type reviewBindingStream struct{ n int }

func (*reviewBindingStream) Close() error { return nil }
func (s *reviewBindingStream) Recv() (llm.StreamEvent, error) {
	s.n++
	switch s.n {
	case 1:
		return llm.StreamEvent{Type: "text_delta", TextDelta: "done"}, nil
	case 2:
		return llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
	default:
		return llm.StreamEvent{}, io.EOF
	}
}
func TestDesktopBindingRevocationRemovesRecallFromProvider(t *testing.T) {
	for _, bound := range []bool{false, true} {
		label := "manager"
		if bound {
			label = "webui-binding"
		}
		t.Run(label, func(t *testing.T) {
			mm, err := memory.NewMemoryManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err = mm.Archival().Insert(memory.Passage{ID: "recall-one", Content: "Nebula deployment uses Copper database.", SourceSessionID: "revoked-source"}); err != nil {
				t.Fatal(err)
			}
			p := &reviewBindingProvider{}
			l := agent.NewLoop(p, tools.NewRegistry(), nil, nil, "", 2)
			l.Memory = mm
			l.AutoRetrieveK = 2
			l.DistillEvery = 0
			if bound {
				if _, err = BindLoopWorkspaceMemory(l, ""); err != nil {
					t.Fatal(err)
				}
			}
			out := make(chan agent.Event, 512)
			l.AppendUser("Nebula deployment")
			if err = l.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			if len(p.requests) == 0 {
				t.Fatal("no first request")
			}
			if err = mm.DeleteSession("revoked-source"); err != nil {
				t.Fatal(err)
			}
			l.AppendUser("continue")
			if err = l.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			last := p.requests[len(p.requests)-1]
			found := false
			for _, m := range last.Messages {
				for _, b := range m.Content {
					if strings.Contains(b.Text, "Nebula deployment uses Copper database.") {
						found = true
					}
				}
			}
			t.Logf("bound=%v requests=%d revoked_body_visible=%v", bound, len(p.requests), found)
			if found {
				t.Fatal("revoked memory was sent to next provider request")
			}
		})
	}
}
