package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type imageAttachmentProbeProvider struct {
	stubProvider
	check func(context.Context) error
	err   error
	calls int
}

func (p *imageAttachmentProbeProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.calls++
	p.err = p.check(ctx)
	return &reviewBindingStream{}, nil
}

func TestImageAttachmentRuntimeBindingSurvivesCompactionRestartAndUIRebind(t *testing.T) {
	oldAdapter := CurrentTraceAdapter()
	SetTraceAdapter(nil)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })
	previousSession := tasks.CurrentSessionID()
	t.Cleanup(func() { tasks.SetCurrentSessionID(previousSession) })
	for _, embedded := range []bool{false, true} {
		t.Run(fmt.Sprintf("embedded=%v", embedded), func(t *testing.T) {
			cfg := defaultLoopCfg(t)
			store, err := session.NewStore(cfg.Session.Dir)
			if err != nil {
				t.Fatal(err)
			}
			const owner = "origin"
			if err := store.WriteHeader(owner, "offline", ""); err != nil {
				t.Fatal(err)
			}
			if err := store.WriteHeader("selected-other", "offline", ""); err != nil {
				t.Fatal(err)
			}
			const encoded = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
			raw, _ := base64.StdEncoding.DecodeString(encoded)
			path := filepath.Join(t.TempDir(), "deleted-original.png")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			message := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", Data: encoded, MediaType: "image/png"}}}
			if err := store.AppendMessage(owner, message); err != nil {
				t.Fatal(err)
			}
			cursor := session.NewHistoryCursor(nil)
			if err := store.ReplaceHistoryAndMark(owner, []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "compacted image reference"}}}}, &cursor); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			store, err = session.NewStore(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			id := blob.Sum(raw)
			p := &imageAttachmentProbeProvider{stubProvider: stubProvider{maxCtx: 100_000}}
			p.check = func(ctx context.Context) error {
				// Model/tool work retains origin after the selected UI session
				// changes, including when no trace adapter is installed.
				tasks.SetCurrentSessionID("selected-other")
				result, err := (builtin.ViewImage{}).Execute(ctx, map[string]any{"attachment_id": id})
				if err != nil {
					return err
				}
				if result.IsError || len(result.Images) != 1 || result.Images[0].MediaType != "image/png" {
					return fmt.Errorf("missing native image: %+v", result)
				}
				got, err := base64.StdEncoding.DecodeString(result.Images[0].Data)
				if err != nil || !bytes.Equal(got, raw) {
					return fmt.Errorf("saved image bytes changed: %v", err)
				}
				return nil
			}
			var loop *agent.Loop
			if embedded {
				loop = agent.NewLoop(p, tools.NewRegistry(), nil, nil, "", 1)
				BindImageAttachmentStore(loop, store)
			} else {
				loop = BuildAgentLoop(cfg, AgentLoopOptions{Provider: p, Registry: tools.NewRegistry(), MaxIter: 1, MemoryManagerProvided: true, ImageAttachments: store})
				loop.Compactor = nil
			}
			loop.DistillEvery = 0
			loop.AppendUser("inspect the saved image")
			tasks.SetCurrentSessionID("selected-other")
			if err := RunWithTraceTurn(context.Background(), owner, func(ctx context.Context) error {
				return loop.Run(ctx, make(chan agent.Event, 128))
			}); err != nil {
				t.Fatal(err)
			}
			if p.calls == 0 {
				t.Fatal("runtime never exercised the attachment binding")
			}
			if p.err != nil {
				t.Fatal(p.err)
			}
		})
	}
}
