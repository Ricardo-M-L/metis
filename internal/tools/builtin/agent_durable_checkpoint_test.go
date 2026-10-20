package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type childCheckpointProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	check    func(llm.Request) error
}

func (*childCheckpointProvider) Name() string          { return "child-checkpoint" }
func (*childCheckpointProvider) ModelID() string       { return "test" }
func (*childCheckpointProvider) MaxContextTokens() int { return 200000 }
func (*childCheckpointProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream only")
}
func (p *childCheckpointProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.check != nil {
		if err := p.check(req); err != nil {
			return nil, err
		}
	}
	return &fakeStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "received"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}, nil
}
func (p *childCheckpointProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func childEnvelopeCount(h []llm.Message) int {
	n := 0
	for _, m := range h {
		for _, b := range m.Content {
			if strings.HasPrefix(b.Text, `<peer_message durable="true">`) {
				n++
			}
		}
	}
	return n
}
func childCheckpointFixture(t *testing.T) (string, *handoff.Store, handoff.Record, *agent.Roster) {
	t.Helper()
	dir := t.TempDir()
	s, err := handoff.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: "workspace-test", From: "main", To: "alice", Body: "unique named child handoff"})
	if err != nil {
		t.Fatal(err)
	}
	roster := agent.NewRoster(0)
	roster.BindHandoff(s, "workspace-test")
	return dir, s, record, roster
}

func TestAgentNamedChildDurableCheckpointBeforeProviderAndResume(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", background), func(t *testing.T) {
			dir, store, record, roster := childCheckpointFixture(t)
			provider := &childCheckpointProvider{}
			provider.check = func(req llm.Request) error {
				row, err := store.Get(context.Background(), record.ID)
				if err != nil || row.State != "acknowledged" {
					return fmt.Errorf("provider before mailbox ack: %#v %v", row, err)
				}
				ids, err := agent.ListSubAgentTranscripts(dir)
				if err != nil || len(ids) != 1 {
					return fmt.Errorf("missing child transcript: %v %v", ids, err)
				}
				snap, err := agent.LoadSubAgentSnapshot(dir, ids[0])
				if err != nil {
					return err
				}
				if childEnvelopeCount(req.Messages) != 1 || childEnvelopeCount(snap.Messages) != 1 {
					return errors.New("handoff absent or duplicated in actual model request/transcript")
				}
				if snap.Handoff == nil || snap.Handoff.Scope != "workspace-test" || snap.Handoff.Recipient != "alice" {
					return errors.New("stable mailbox address not persisted")
				}
				if provider.count() == 1 {
					encoded, err := json.Marshal(snap.Messages)
					if err != nil {
						return err
					}
					sum := sha256.Sum256(encoded)
					if row.CheckpointSHA256 != hex.EncodeToString(sum[:]) {
						return errors.New("receipt not exact producer checkpoint")
					}
				}
				return nil
			}
			tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "m", "system").WithRoster(roster).WithSessionPersistence(dir, "parent")
			result, err := tool.Execute(context.Background(), map[string]any{"prompt": "process mail", "name": "alice", "run_in_background": background})
			if err != nil || result.IsError {
				t.Fatalf("child=%#v %v", result, err)
			}
			if background {
				waitForRosterCount(t, roster, 0, 3*time.Second)
				id, _ := result.Meta["agent_id"].(string)
				teammate, ok := roster.LookupByAgentID(id)
				if !ok {
					t.Fatal("background child not in roster")
				}
				if teammate.Snapshot().Status != agent.StatusCompleted {
					t.Fatalf("background result=%#v", teammate.Snapshot())
				}
			}
			ids, err := agent.ListSubAgentTranscripts(dir)
			if err != nil || len(ids) != 1 {
				t.Fatalf("ids=%v %v", ids, err)
			}
			before, err := os.ReadFile(filepath.Join(dir, agent.SubAgentTranscriptDirname, ids[0]+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(before), `"type":"message"`) {
				t.Fatal("consumer appended duplicate live history in durable mode")
			}
			// A new runtime recreates the same scope/name and resumes the same transcript.
			reopenedRoster := agent.NewRoster(0)
			reopenedRoster.BindHandoff(store, "workspace-test")
			tool = tool.WithRoster(reopenedRoster)
			result, err = tool.Execute(context.Background(), map[string]any{"prompt": "continue", "resume_from": ids[0]})
			if err != nil || result.IsError {
				t.Fatalf("resume=%#v %v", result, err)
			}
			after, err := os.ReadFile(filepath.Join(dir, agent.SubAgentTranscriptDirname, ids[0]+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(after), string(before)) {
				t.Fatal("resume rewrote acknowledged transcript")
			}
			snap, err := agent.LoadSubAgentSnapshot(dir, ids[0])
			if err != nil || childEnvelopeCount(snap.Messages) != 1 || len(snap.Messages) != 5 {
				t.Fatalf("recovered history=%#v %v", snap, err)
			}
			if provider.count() != 2 {
				t.Fatalf("provider calls=%d", provider.count())
			}
		})
	}
}

func TestAgentDurableChildTranscriptFailureLeavesMailPending(t *testing.T) {
	dir, store, record, roster := childCheckpointFixture(t)
	badPath := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(badPath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &childCheckpointProvider{}
	tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "m", "system").WithRoster(roster).WithSessionPersistence(badPath, "parent")
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "process", "name": "alice"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "transcript setup failed") {
		t.Fatalf("setup failure=%#v %v", result, err)
	}
	if provider.count() != 0 || roster.Count() != 0 {
		t.Fatal("failed transcript launched or leaked child")
	}
	row, err := store.Get(context.Background(), record.ID)
	if err != nil || row.State != "pending" || row.Attempts != 0 {
		t.Fatalf("setup failure claimed mail: %#v %v", row, err)
	}
}

func TestAgentDurableChildResumeCannotChangeMailbox(t *testing.T) {
	dir, store, _, roster := childCheckpointFixture(t)
	provider := &childCheckpointProvider{}
	tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "m", "system").WithRoster(roster).WithSessionPersistence(dir, "parent")
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "process", "name": "alice"})
	if err != nil || result.IsError {
		t.Fatalf("setup=%#v %v", result, err)
	}
	ids, _ := agent.ListSubAgentTranscripts(dir)
	before, _ := os.ReadFile(filepath.Join(dir, agent.SubAgentTranscriptDirname, ids[0]+".jsonl"))
	for _, test := range []struct {
		name, scope string
		bound       bool
	}{{"bob", "workspace-test", true}, {"alice", "other-scope", true}, {"alice", "", false}} {
		newRoster := agent.NewRoster(0)
		if test.bound {
			newRoster.BindHandoff(store, test.scope)
		}
		result, err := tool.WithRoster(newRoster).Execute(context.Background(), map[string]any{"prompt": "resume", "name": test.name, "resume_from": ids[0]})
		if err != nil || !result.IsError || !strings.Contains(result.Output, "original mailbox") {
			t.Fatalf("changed mailbox allowed: %#v %v", result, err)
		}
		if newRoster.Count() != 0 {
			t.Fatal("refused resume consumed roster slot")
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, agent.SubAgentTranscriptDirname, ids[0]+".jsonl"))
	if !reflect.DeepEqual(before, after) || provider.count() != 1 {
		t.Fatal("refused resume changed acknowledged history or called provider")
	}
}

func TestAgentDurableNamedChildRequiresPersistenceAnonymousStillWorks(t *testing.T) {
	_, store, record, roster := childCheckpointFixture(t)
	provider := &childCheckpointProvider{}
	tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "m", "system").WithRoster(roster)
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "process", "name": "alice"})
	if err != nil || !result.IsError || provider.count() != 0 {
		t.Fatalf("unpersisted named child=%#v %v", result, err)
	}
	result, err = tool.Execute(context.Background(), map[string]any{"prompt": "anonymous"})
	if err != nil || result.IsError || provider.count() != 1 {
		t.Fatalf("anonymous compatibility=%#v %v", result, err)
	}
	row, err := store.Get(context.Background(), record.ID)
	if err != nil || row.State != "pending" || row.Attempts != 0 {
		t.Fatalf("anonymous child claimed named mailbox: %#v %v", row, err)
	}
}
