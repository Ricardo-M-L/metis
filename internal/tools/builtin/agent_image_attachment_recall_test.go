package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type childImageRecallProvider struct {
	path, id, encoded string
	step              int
	recallOnly        bool
	err               error
	sawNative         int
}

func (*childImageRecallProvider) Name() string          { return "child-image-recall" }
func (*childImageRecallProvider) ModelID() string       { return "offline-vision" }
func (*childImageRecallProvider) MaxContextTokens() int { return 200000 }
func (*childImageRecallProvider) SupportsVision() bool  { return true }
func (*childImageRecallProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream only")
}
func (p *childImageRecallProvider) Stream(_ context.Context, request llm.Request) (llm.StreamReader, error) {
	if p.step > 0 {
		var result *llm.ContentBlock
		for _, message := range request.Messages {
			for i := range message.Content {
				if message.Content[i].Type == "tool_result" {
					block := message.Content[i]
					result = &block
				}
			}
		}
		if result == nil || result.IsError || len(result.ToolResultBlocks) < 2 {
			p.err = fmt.Errorf("child native image missing at step %d: %+v", p.step, result)
		} else {
			image := result.ToolResultBlocks[len(result.ToolResultBlocks)-1]
			if image.Type != "image" || image.Data != p.encoded || image.AttachmentHash != p.id {
				p.err = fmt.Errorf("child native image or canonical ID changed at step %d", p.step)
			} else {
				p.sawNative++
			}
		}
	}
	if p.step == 0 && p.recallOnly {
		for _, message := range request.Messages {
			for _, block := range message.Content {
				if block.Type == "image" || len(block.ToolResultBlocks) > 0 {
					p.err = errors.New("child restart replayed pre-compaction image")
				}
			}
		}
	}
	input := map[string]any{"attachment_id": p.id}
	if p.step == 0 && !p.recallOnly {
		input = map[string]any{"path": p.path}
	} else if p.step == 1 && !p.recallOnly {
		if err := os.Remove(p.path); err != nil {
			p.err = err
		}
	}
	finished := p.step >= 2 || (p.recallOnly && p.step >= 1)
	p.step++
	if finished {
		return &fakeStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "inspected"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}, nil
	}
	encodedInput, _ := json.Marshal(input)
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: fmt.Sprintf("image-%d", p.step), ToolName: "ViewImage"},
		{Type: "tool_input_delta", InputDelta: string(encodedInput)},
		{Type: "tool_use_stop", InputDelta: string(encodedInput)},
		{Type: "message_delta", StopReason: "tool_use"}, {Type: "message_stop"},
	}}, nil
}

func TestAgentChildImageAttachmentRecallSurvivesCheckpointAndRestart(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner", "foreign"} {
		if err := store.WriteHeader(id, "offline-vision", ""); err != nil {
			t.Fatal(err)
		}
	}
	path := writeTinyPNG(t, t.TempDir(), "child-only.png")
	raw, _ := os.ReadFile(path)
	id, encoded := blob.Sum(raw), base64.StdEncoding.EncodeToString(raw)
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(ViewImage{gate: gate})
	ctx := tasks.WithSessionID(context.Background(), "owner")
	ctx = agent.WithImageAttachmentSession(ctx, store, "owner")
	ctx = tools.WithInvocationID(ctx, "parent-agent-call")
	p := &childImageRecallProvider{path: path, id: id, encoded: encoded}
	child := NewAgent(gate, p, registry, "offline-vision", "system").WithRoster(agent.NewRoster(2)).WithSessionPersistence(store.Dir, "owner")
	agentID := executeUsageAgent(t, ctx, child, map[string]any{"prompt": "inspect the temporary image, then inspect its stable ID"})
	if p.err != nil || p.sawNative != 2 {
		t.Fatalf("child stable-ID recall: %v (native=%d)", p.err, p.sawNative)
	}
	ledger, err := os.ReadFile(filepath.Join(store.Dir, "owner.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	registrations := 0
	for _, line := range bytes.Split(bytes.TrimSpace(ledger), []byte("\n")) {
		var entry session.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == "image_attachment" && entry.Message != nil {
			for _, image := range entry.Message.Content {
				if image.TraceCallID != "parent-agent-call" {
					t.Fatalf("child image lost root occurrence: %q", image.TraceCallID)
				}
				registrations++
			}
		}
	}
	if registrations != 2 {
		t.Fatalf("child native images registered %d times, want 2", registrations)
	}
	// Use the child's durable history replacement primitive to model a compact
	// checkpoint that removes both original native images before restarting.
	transcript, err := agent.ResumeSubAgentTranscript(store.Dir, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Checkpoint([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "compacted image attachment_id=" + id}}}}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := session.NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithImageAttachmentSession(tasks.WithSessionID(context.Background(), "owner"), restarted, "owner")
	resumedProvider := &childImageRecallProvider{id: id, encoded: encoded, recallOnly: true}
	resumed := NewAgent(gate, resumedProvider, registry, "offline-vision", "system").WithRoster(agent.NewRoster(2)).WithSessionPersistence(restarted.Dir, "owner")
	if got := executeUsageAgent(t, ctx, resumed, map[string]any{"prompt": "inspect the retained attachment", "resume_from": agentID}); got != agentID {
		t.Fatalf("resumed child changed identity: %s", got)
	}
	if resumedProvider.err != nil || resumedProvider.sawNative != 1 {
		t.Fatalf("restarted child native recall: %v (native=%d)", resumedProvider.err, resumedProvider.sawNative)
	}
	foreign := agent.WithImageAttachmentSession(context.Background(), restarted, "foreign")
	if result, err := (ViewImage{}).Execute(foreign, map[string]any{"attachment_id": id}); err == nil {
		t.Fatalf("foreign root read child image: %+v", result)
	}
	_, rootHistory, err := restarted.Load("owner")
	if err != nil || len(rootHistory) != 0 {
		t.Fatalf("attachment registration changed root conversation: %+v, %v", rootHistory, err)
	}
}
