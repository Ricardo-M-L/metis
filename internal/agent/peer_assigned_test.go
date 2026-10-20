package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func assignedPeerFixture(t *testing.T) (*handoff.Store, handoff.Record, *DurablePeerInbox) {
	t.Helper()
	s, err := handoff.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "requester", To: "job:receiver", Body: "assigned-input", Task: &handoff.Task{ID: "task-1", Objective: "process only this assignment"}})
	if err != nil {
		t.Fatal(err)
	}
	a := handoff.BusinessAssignment{JobID: "receiver", Workspace: "/workspace", RunID: "run-1", WorkItemID: "handoff-" + r.ID, VerificationItemID: "handoff-" + r.ID + "-verify", ConfigSHA256: strings.Repeat("c", 64)}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, handoff.BusinessUpdate{Assignment: a, State: handoff.BusinessAccepted}); err != nil {
		t.Fatal(err)
	}
	b, err := NewDurablePeerInbox(s, "jobs", "job:receiver")
	if err != nil {
		t.Fatal(err)
	}
	b.MessageID, b.Assignment, b.ContentSHA256 = r.ID, &a, r.ContentSHA256
	return s, r, b
}

func TestAssignedDurablePeerConsumesOnlyItsMessage(t *testing.T) {
	s, assigned, b := assignedPeerFixture(t)
	other, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "requester", To: b.Recipient, Body: "other-structured-task", Task: &handoff.Task{ID: "task-2", Objective: "different assignment"}})
	if err != nil {
		t.Fatal(err)
	}
	otherAssignment := *b.Assignment
	otherAssignment.WorkItemID = "handoff-" + other.ID
	otherAssignment.VerificationItemID = otherAssignment.WorkItemID + "-verify"
	if _, err := s.ReconcileBusiness(context.Background(), other.ID, handoff.BusinessUpdate{Assignment: otherAssignment, State: handoff.BusinessAccepted}); err != nil {
		t.Fatal(err)
	}
	notification, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "requester", To: b.Recipient, Body: "unrelated-notification"})
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{DurablePeer: b, HistoryCheckpoint: durableTestCheckpoint(t, filepath.Join(t.TempDir(), "history.json"))}
	for n := 0; n < 3; n++ {
		if err := l.injectPeerMessages(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if historyEnvelopeCount(l.History()) != 1 || !historyHasEnvelope(l.History(), formatDurablePeerMessage(assigned.Envelope)) {
		t.Fatalf("exact assignment did not isolate its input: %#v", l.History())
	}
	for _, record := range []handoff.Record{other, notification} {
		got, err := s.Get(context.Background(), record.ID)
		if err != nil || got.State != "pending" || got.Attempts != 0 {
			t.Fatalf("unrelated message consumed: %#v, %v", got, err)
		}
	}
	got, err := s.Get(context.Background(), assigned.ID)
	if err != nil || got.State != "acknowledged" || got.Attempts != 1 {
		t.Fatalf("assigned delivery duplicated: %#v, %v", got, err)
	}
}

func TestAssignedDurablePeerRejectsWrongContractBeforeProvider(t *testing.T) {
	for name, mutate := range map[string]func(*DurablePeerInbox){
		"message":    func(b *DurablePeerInbox) { b.MessageID = "msg-" + strings.Repeat("0", 64) },
		"recipient":  func(b *DurablePeerInbox) { b.Recipient = "job:another" },
		"digest":     func(b *DurablePeerInbox) { b.ContentSHA256 = strings.Repeat("0", 64) },
		"job":        func(b *DurablePeerInbox) { b.Assignment.JobID = "another" },
		"workspace":  func(b *DurablePeerInbox) { b.Assignment.Workspace = "/different" },
		"run":        func(b *DurablePeerInbox) { b.Assignment.RunID = "run-other" },
		"config":     func(b *DurablePeerInbox) { b.Assignment.ConfigSHA256 = strings.Repeat("d", 64) },
		"item":       func(b *DurablePeerInbox) { b.Assignment.WorkItemID = "different" },
		"incomplete": func(b *DurablePeerInbox) { b.Assignment = nil },
		"mixed-mode": func(b *DurablePeerInbox) { b.NotificationsOnly = true },
	} {
		t.Run(name, func(t *testing.T) {
			s, record, b := assignedPeerFixture(t)
			mutate(b)
			p := &durablePeerProvider{}
			l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
			l.DurablePeer, l.HistoryCheckpoint = b, durableTestCheckpoint(t, filepath.Join(t.TempDir(), "history.json"))
			l.AppendUser("process assignment")
			if err := l.Run(context.Background(), make(chan Event, 128)); err == nil {
				t.Fatal("mismatched assignment reached the model")
			}
			if len(p.calls) != 0 || historyEnvelopeCount(l.History()) != 0 {
				t.Fatal("invalid contract was injected or dispatched")
			}
			got, err := s.Get(context.Background(), record.ID)
			if err != nil || got.State != "pending" || got.CheckpointSHA256 != "" {
				t.Fatalf("invalid contract was acknowledged: %#v %v", got, err)
			}
		})
	}
}

func TestAssignedDurablePeerRestartAfterCheckpointBeforeACK(t *testing.T) {
	s, r, b := assignedPeerFixture(t)
	path := filepath.Join(t.TempDir(), "history.json")
	checkpoint := durableTestCheckpoint(t, path)
	first := &Loop{DurablePeer: b}
	first.HistoryCheckpoint = func(h []llm.Message) error {
		if err := checkpoint(h); err != nil {
			return err
		}
		claimed, err := s.Get(context.Background(), r.ID)
		if err != nil {
			return err
		}
		return s.Release(context.Background(), r.ID, claimed.Token)
	}
	if err := first.injectPeerMessages(context.Background(), nil); !errors.Is(err, handoff.ErrLease) {
		t.Fatalf("simulated crash error=%v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var history []llm.Message
	if err := json.Unmarshal(saved, &history); err != nil {
		t.Fatal(err)
	}
	newInbox, err := NewDurablePeerInbox(s, b.Scope, b.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	newInbox.MessageID, newInbox.Assignment, newInbox.ContentSHA256 = b.MessageID, b.Assignment, b.ContentSHA256
	restored := &Loop{Messages: history, DurablePeer: newInbox, HistoryCheckpoint: checkpoint}
	events := make(chan Event, 32)
	if err := restored.injectPeerMessages(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if historyEnvelopeCount(restored.History()) != 1 {
		t.Fatal("restart duplicated assigned envelope")
	}
	for len(events) > 0 {
		if (<-events).Kind == EventContextInjected {
			t.Fatal("restart emitted a duplicate injection")
		}
	}
	if err := restored.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), r.ID)
	if err != nil || got.State != "acknowledged" || got.Attempts != 2 {
		t.Fatalf("bad recovery receipt: %#v %v", got, err)
	}
	// An ACK from this transcript must not authorize an unrelated/lost history.
	p := &durablePeerProvider{}
	lost := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	lost.DurablePeer, lost.HistoryCheckpoint = newInbox, checkpoint
	lost.AppendUser("different transcript")
	if err := lost.Run(context.Background(), make(chan Event, 128)); err == nil || !strings.Contains(err.Error(), "missing from the restored transcript") {
		t.Fatalf("missing acknowledged input allowed: %v", err)
	}
	if len(p.calls) != 0 {
		t.Fatal("provider ran without acknowledged input")
	}
}

func TestAssignedDurablePeerDoesNotRunWhileAnotherConsumerOwnsDelivery(t *testing.T) {
	s, r, b := assignedPeerFixture(t)
	if _, err := s.ClaimID(context.Background(), r.ID, b.Scope, b.Recipient, "other-consumer", time.Minute, *b.Assignment); err != nil {
		t.Fatal(err)
	}
	l := &Loop{DurablePeer: b, HistoryCheckpoint: durableTestCheckpoint(t, filepath.Join(t.TempDir(), "history.json"))}
	if err := l.injectPeerMessages(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "another delivery consumer") {
		t.Fatalf("busy delivery was silently skipped: %v", err)
	}
}

func TestNotificationsOnlyLeavesAllStructuredTasksForDispatcher(t *testing.T) {
	s, assigned, b := assignedPeerFixture(t)
	unassigned, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: b.Scope, From: "requester", To: b.Recipient, Body: "unassigned work", Task: &handoff.Task{ID: "task-2", Objective: "dispatch me separately"}})
	if err != nil {
		t.Fatal(err)
	}
	notification, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: b.Scope, From: "requester", To: b.Recipient, Body: "notification only"})
	if err != nil {
		t.Fatal(err)
	}
	b.MessageID, b.Assignment, b.ContentSHA256, b.NotificationsOnly = "", nil, "", true
	l := &Loop{DurablePeer: b, HistoryCheckpoint: durableTestCheckpoint(t, filepath.Join(t.TempDir(), "history.json"))}
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if historyEnvelopeCount(l.History()) != 1 || !historyHasEnvelope(l.History(), formatDurablePeerMessage(notification.Envelope)) {
		t.Fatal("notification-only worker received wrong input")
	}
	for _, input := range []handoff.Record{assigned, unassigned} {
		row, err := s.Get(context.Background(), input.ID)
		if err != nil || row.State != "pending" || row.Attempts != 0 {
			t.Fatalf("structured task consumed as a notification: %#v %v", row, err)
		}
	}
}

func TestDefaultNamedChildInboxStillAcceptsUnassignedStructuredMessage(t *testing.T) {
	s, err := handoff.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Send(context.Background(), handoff.SendRequest{Scope: "session:one", From: "alice", To: "bob", Body: "named child input", Task: &handoff.Task{ID: "task", Objective: "legacy manual structured delivery"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDurablePeerInbox(s, r.Scope, r.To)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{DurablePeer: b, HistoryCheckpoint: durableTestCheckpoint(t, filepath.Join(t.TempDir(), "history.json"))}
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !historyHasEnvelope(l.History(), formatDurablePeerMessage(r.Envelope)) {
		t.Fatal("default named-child delivery semantics changed")
	}
}
