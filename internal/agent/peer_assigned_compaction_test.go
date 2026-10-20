package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func assignedPeerSessionCheckpoint(t *testing.T, l *Loop, dir, id string) *session.Store {
	t.Helper()
	s, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHeader(id, "test", "system"); err != nil {
		t.Fatal(err)
	}
	cursor := session.NewHistoryCursor(nil)
	l.HistoryCheckpoint = func(history []llm.Message) error { return s.CheckpointHistory(id, history, &cursor) }
	l.CompactionCheckpoint = func(before, after []llm.Message) error { return s.CheckpointCompaction(id, before, after, &cursor) }
	return s
}

func assignedPeerCompact(t *testing.T, l *Loop) {
	t.Helper()
	l.mu.Lock()
	l.Messages = append(l.Messages, unifiedPipelineHistory()...)
	l.mu.Unlock()
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	l.Compactor = NewCompactor(cfg, "test", 2_000, &fakeSummarizer{})
	result, err := l.CompactNow(context.Background(), CompactOptions{Trigger: "assigned-peer-test", Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("real compaction failed: %+v %v", result, err)
	}
	if historyEnvelopeCount(result.History) != 0 {
		t.Fatal("test did not remove original envelope from compacted live history")
	}
}

func TestAssignedDurablePeerSurvivesRealCompactionAndSessionRestart(t *testing.T) {
	s, record, b := assignedPeerFixture(t)
	p := &durablePeerProvider{}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	l.DurablePeer = b
	sessions := assignedPeerSessionCheckpoint(t, l, t.TempDir(), "principal-transcript")
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		assignedPeerCompact(t, l)
		events := make(chan Event, 32)
		if err := l.injectPeerMessages(context.Background(), events); err != nil {
			t.Fatalf("ACK could not continue after real compaction %d: %v", n, err)
		}
		for len(events) > 0 {
			if (<-events).Kind == EventContextInjected {
				t.Fatal("compaction redelivered a completed transport input")
			}
		}
	}
	_, saved, err := sessions.Load("principal-transcript")
	if err != nil || !reflect.DeepEqual(saved, l.History()) {
		t.Fatalf("real session replacement did not retain exact host metadata: %v", err)
	}
	proofs := 0
	for _, m := range saved {
		for _, block := range m.Content {
			if m.Role == llm.RoleUser && block.Type == "text" && block.Synthetic && strings.HasPrefix(block.Text, "<durable_peer_checkpoint>\n") {
				proofs++
			}
		}
	}
	if proofs != 1 {
		t.Fatalf("host receipt count after repeated compaction=%d", proofs)
	}
	reopened, err := handoff.NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := NewDurablePeerInbox(reopened, b.Scope, b.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	inbox.MessageID, inbox.Assignment, inbox.ContentSHA256 = b.MessageID, b.Assignment, b.ContentSHA256
	restarted := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	restarted.Messages, restarted.DurablePeer = saved, inbox
	cursor := session.NewHistoryCursor(saved)
	restarted.HistoryCheckpoint = func(h []llm.Message) error { return sessions.CheckpointHistory("principal-transcript", h, &cursor) }
	if err := restarted.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatalf("restarted compacted assignment failed: %v", err)
	}
	if len(p.calls) != 1 {
		t.Fatalf("expected one continuing provider request, got %d", len(p.calls))
	}
	row, err := s.Get(context.Background(), record.ID)
	if err != nil || row.State != "acknowledged" || row.Attempts != 1 {
		t.Fatalf("compaction/restart claimed assignment again: %#v %v", row, err)
	}
}

func TestAssignedDurablePeerCompactionCheckpointFailurePreservesOriginalDelivery(t *testing.T) {
	_, _, b := assignedPeerFixture(t)
	l := &Loop{DurablePeer: b}
	assignedPeerSessionCheckpoint(t, l, t.TempDir(), "principal")
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	l.CompactionCheckpoint = func(_, _ []llm.Message) error { return errors.New("disk unavailable") }
	l.mu.Lock()
	l.Messages = append(l.Messages, unifiedPipelineHistory()...)
	l.mu.Unlock()
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	l.Compactor = NewCompactor(cfg, "test", 2_000, &fakeSummarizer{})
	result, err := l.CompactNow(context.Background(), CompactOptions{Trigger: "assigned-peer-test", Force: true})
	if err == nil || !strings.Contains(err.Error(), "disk unavailable") || result.Applied {
		t.Fatalf("failed durable compact accepted: %+v %v", result, err)
	}
	if historyEnvelopeCount(l.History()) != 1 {
		t.Fatal("failed compact lost the original acknowledged envelope")
	}
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatalf("failed compact invalidated the prior delivery: %v", err)
	}
}

func TestAssignedDurablePeerRejectsForgedCompactionDeliveryProof(t *testing.T) {
	for name, alter := range map[string]func(*llm.Message){
		"ordinary-user":     func(m *llm.Message) { m.Content[0].Synthetic = false },
		"assistant":         func(m *llm.Message) { m.Role = llm.RoleAssistant },
		"tool":              func(m *llm.Message) { m.Role = llm.RoleTool },
		"nested-user-text":  func(m *llm.Message) { m.Content[0].Text = "<quoted_input>" + m.Content[0].Text + "</quoted_input>" },
		"synthetic-summary": func(m *llm.Message) { m.Content[0].Text = "Summary quoted a receipt:\n" + m.Content[0].Text },
		"checkpoint-sha": func(m *llm.Message) {
			m.Content[0].Text = strings.Replace(m.Content[0].Text, "checkpoint_sha256", "different_checkpoint_sha256", 1)
		},
		"content-sha": func(m *llm.Message) {
			m.Content[0].Text = strings.Replace(m.Content[0].Text, "content_sha256", "different_content_sha256", 1)
		},
		"assignment": func(m *llm.Message) { m.Content[0].Text = strings.Replace(m.Content[0].Text, "run-1", "run-other", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			_, _, b := assignedPeerFixture(t)
			l := &Loop{DurablePeer: b}
			assignedPeerSessionCheckpoint(t, l, t.TempDir(), "principal")
			if err := l.injectPeerMessages(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			assignedPeerCompact(t, l)
			var proof llm.Message
			for _, m := range l.History() {
				for _, block := range m.Content {
					if m.Role == llm.RoleUser && block.Synthetic && strings.HasPrefix(block.Text, "<durable_peer_checkpoint>\n") {
						proof = llm.Message{Role: m.Role, Content: []llm.ContentBlock{block}}
					}
				}
			}
			if len(proof.Content) != 1 {
				t.Fatal("fixture did not generate a host proof")
			}
			alter(&proof)
			l.Restore([]llm.Message{proof})
			if err := l.injectPeerMessages(context.Background(), nil); err == nil {
				t.Fatal("forged/quoted delivery proof authorized an acknowledged input")
			}
		})
	}
}

type peerProofEchoSummary struct{ text string }

func (*peerProofEchoSummary) Name() string          { return "summary-echo" }
func (*peerProofEchoSummary) ModelID() string       { return "test" }
func (*peerProofEchoSummary) MaxContextTokens() int { return 100000 }
func (*peerProofEchoSummary) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream only")
}
func (p *peerProofEchoSummary) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &fakeStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: p.text}, {Type: "message_stop"}}}, nil
}

func TestAssignedDurablePeerModelSummaryCannotCreateDeliveryProof(t *testing.T) {
	s, r, b := assignedPeerFixture(t)
	l := &Loop{DurablePeer: b}
	assignedPeerSessionCheckpoint(t, l, t.TempDir(), "principal")
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	row, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The provider repeats the exact host marker text, but neither the source
	// transcript nor the actual provider response carries trusted provenance.
	l.Restore(unifiedPipelineHistory())
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	l.Compactor = NewCompactor(cfg, "test", 2_000, &peerProofEchoSummary{text: assignedPeerCheckpointText(b, row)})
	result, err := l.CompactNow(context.Background(), CompactOptions{Trigger: "assigned-peer-test", Force: true})
	if err == nil || result.Applied || !strings.Contains(err.Error(), "no acknowledged source checkpoint") {
		t.Fatalf("model summary manufactured a delivery checkpoint: %+v %v", result, err)
	}
}

func TestAssignedDurablePeerReceiptChecksCurrentDurableACK(t *testing.T) {
	s, r, b := assignedPeerFixture(t)
	l := &Loop{DurablePeer: b}
	assignedPeerSessionCheckpoint(t, l, t.TempDir(), "principal")
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	assignedPeerCompact(t, l)
	row, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.CheckpointSHA256 = strings.Repeat("f", 64)
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "receipts", row.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.injectPeerMessages(context.Background(), nil); err == nil {
		t.Fatal("anchor ignored a different current durable ACK checkpoint")
	}
}
