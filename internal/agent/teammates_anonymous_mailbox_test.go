package agent

import (
	"context"
	"testing"
)

// TestRoster_AnonymousGetsNoMailbox — claude-code 架构图 image 3 hard
// constraint: sub-agents must not communicate. Pre-2026-05-16 metis
// enforced this only at the description layer (MessageTeammate's tool
// description told the model "anonymous sub-agents cannot receive peer
// messages"). A determined model could still try and silently land
// messages in the mailbox. The fix moves the refusal to type level:
// anonymous teammates have Mailbox == nil, and MessageTeammate
// rejects nil-mailbox recipients explicitly.
func TestRoster_AnonymousGetsNoMailbox(t *testing.T) {
	r := NewRoster(10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tm := &Teammate{
		Name:      "_anon-deadbeef",
		AgentID:   "agt-12345678",
		Anonymous: true,
		Cancel:    cancel,
	}
	_ = ctx
	if err := r.Register(tm); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if tm.Mailbox != nil {
		t.Errorf("anonymous teammate got Mailbox %v; expected nil to enforce no-communication constraint", tm.Mailbox)
	}
}

// TestRoster_NamedGetsMailbox — the inverse: named teammates DO get
// a mailbox because they're part of the Agent-Team paradigm where
// peer messaging is the whole point.
func TestRoster_NamedGetsMailbox(t *testing.T) {
	r := NewRoster(10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tm := &Teammate{
		Name:    "alice",
		AgentID: "agt-aaaa1111",
		Cancel:  cancel,
	}
	_ = ctx
	if err := r.Register(tm); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if tm.Mailbox == nil {
		t.Errorf("named teammate got nil Mailbox; named teammates must support peer messaging")
	}
	if cap(tm.Mailbox) != 16 {
		t.Errorf("named teammate Mailbox capacity = %d, expected 16 (buffered)", cap(tm.Mailbox))
	}
}
