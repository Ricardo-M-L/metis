package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type durablePeerProvider struct {
	calls []llm.Request
	check func(llm.Request)
}

func (*durablePeerProvider) Name() string          { return "durable-peer-test" }
func (*durablePeerProvider) ModelID() string       { return "test" }
func (*durablePeerProvider) MaxContextTokens() int { return 200000 }
func (*durablePeerProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stream only")
}
func (p *durablePeerProvider) Stream(_ context.Context, r llm.Request) (llm.StreamReader, error) {
	p.calls = append(p.calls, r)
	if p.check != nil {
		p.check(r)
	}
	return &activeContextTestStream{events: activeContextResponse("processed", 100, 0, 0, 5)}, nil
}

func durablePeerFixture(t *testing.T) (*handoff.Store, handoff.Record, *DurablePeerInbox) {
	t.Helper()
	s, err := handoff.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "job:sender", To: "job:receiver", Body: "unique-handoff-nonce"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDurablePeerInbox(s, "jobs", "job:receiver")
	if err != nil {
		t.Fatal(err)
	}
	return s, r, b
}
func durableTestCheckpoint(t *testing.T, path string) func([]llm.Message) error {
	t.Helper()
	return func(h []llm.Message) error {
		b, err := json.Marshal(h)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err = f.Write(b); err != nil {
			f.Close()
			return err
		}
		err = f.Sync()
		return errors.Join(err, f.Close())
	}
}
func historyEnvelopeCount(h []llm.Message) int {
	n := 0
	for _, m := range h {
		for _, b := range m.Content {
			if strings.HasPrefix(b.Text, "<peer_message durable=\"true\">") {
				n++
			}
		}
	}
	return n
}

func TestDurablePeerQueryUsesCheckpointBeforeProvider(t *testing.T) {
	s, r, b := durablePeerFixture(t)
	path := filepath.Join(t.TempDir(), "history.json")
	p := &durablePeerProvider{check: func(req llm.Request) {
		row, err := s.Get(context.Background(), r.ID)
		if err != nil || row.State != "acknowledged" {
			t.Fatalf("model called before ack: %#v %v", row, err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(saved)
		if row.CheckpointSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("receipt does not identify exact persisted checkpoint")
		}
		if historyEnvelopeCount(req.Messages) != 1 {
			t.Fatalf("model request envelope count=%d", historyEnvelopeCount(req.Messages))
		}
	}}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	l.DurablePeer = b
	l.HistoryCheckpoint = durableTestCheckpoint(t, path)
	l.AppendUser("process queued handoffs")
	if err := l.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 1 {
		t.Fatalf("provider calls=%d", len(p.calls))
	}
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if historyEnvelopeCount(l.History()) != 1 {
		t.Fatal("acked message was injected again")
	}
}

func TestDurablePeerCheckpointFailureRollsBackAndStopsProvider(t *testing.T) {
	s, r, b := durablePeerFixture(t)
	p := &durablePeerProvider{}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	l.DurablePeer = b
	l.AppendUser("process mail")
	l.HistoryCheckpoint = func(h []llm.Message) error {
		if historyEnvelopeCount(h) > 0 {
			l.AppendUser("concurrent user append")
			return errors.New("disk unavailable")
		}
		return nil
	}
	err := l.Run(context.Background(), make(chan Event, 128))
	if err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("error=%v", err)
	}
	if len(p.calls) != 0 {
		t.Fatal("provider received an unsaved peer message")
	}
	if historyEnvelopeCount(l.History()) != 0 {
		t.Fatal("failed injection left in memory")
	}
	if got := l.History(); len(got) != 2 || got[1].Content[0].Text != "concurrent user append" {
		t.Fatalf("rollback erased unrelated message: %#v", got)
	}
	row, err := s.Get(context.Background(), r.ID)
	if err != nil || row.State != "pending" || row.CheckpointSHA256 != "" {
		t.Fatalf("failed checkpoint acknowledged: %#v %v", row, err)
	}
}

func TestDurablePeerCheckpointThenCrashBeforeAckDeduplicatesOnRestart(t *testing.T) {
	s, r, b := durablePeerFixture(t)
	path := filepath.Join(t.TempDir(), "history.json")
	first := &Loop{DurablePeer: b}
	checkpoint := durableTestCheckpoint(t, path)
	first.HistoryCheckpoint = func(h []llm.Message) error {
		if err := checkpoint(h); err != nil {
			return err
		}
		row, err := s.Get(context.Background(), r.ID)
		if err != nil {
			return err
		}
		return s.Release(context.Background(), row.ID, row.Token)
	}
	if err := first.injectPeerMessages(context.Background(), nil); !errors.Is(err, handoff.ErrLease) {
		t.Fatalf("simulated lost ack error=%v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var history []llm.Message
	if err = json.Unmarshal(saved, &history); err != nil {
		t.Fatal(err)
	}
	reopened, err := handoff.NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewDurablePeerInbox(reopened, "jobs", "job:receiver")
	if err != nil {
		t.Fatal(err)
	}
	restored := &Loop{Messages: history, DurablePeer: binding, HistoryCheckpoint: checkpoint}
	events := make(chan Event, 8)
	if err := restored.injectPeerMessages(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if historyEnvelopeCount(restored.History()) != 1 {
		t.Fatal("restart duplicated checkpointed envelope")
	}
	for len(events) > 0 {
		if (<-events).Kind == EventContextInjected {
			t.Fatal("recovery re-emitted a duplicate injection")
		}
	}
	row, err := s.Get(context.Background(), r.ID)
	if err != nil || row.State != "acknowledged" || row.Attempts != 2 {
		t.Fatalf("receipt=%#v %v", row, err)
	}
}

func TestDurablePeerNoCheckpointFailsClosed(t *testing.T) {
	s, r, b := durablePeerFixture(t)
	p := &durablePeerProvider{}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	l.DurablePeer = b
	l.AppendUser("mail")
	if err := l.Run(context.Background(), make(chan Event, 128)); err == nil {
		t.Fatal("memory-only receiver accepted durable mail")
	}
	if len(p.calls) != 0 {
		t.Fatal("provider ran without durable history")
	}
	row, err := s.Get(context.Background(), r.ID)
	if err != nil || row.State != "pending" || row.CheckpointSHA256 != "" {
		t.Fatalf("mail was claimed without checkpoint: %#v %v", row, err)
	}
}

func TestDurablePeerEmptyMailboxPreservesMemoryOnlyEmbedding(t *testing.T) {
	s, err := handoff.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewDurablePeerInbox(s, "s", "bob")
	if err != nil {
		t.Fatal(err)
	}
	p := &durablePeerProvider{}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	l.DurablePeer = binding
	l.AppendUser("ordinary query")
	if err = l.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 1 {
		t.Fatalf("empty durable inbox blocked query: calls=%d", len(p.calls))
	}
}
