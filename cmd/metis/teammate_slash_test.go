package main

// teammate_slash_test.go — locks /teammate slash command (2026-05-16).
// CC 架构图 image 5 "user → teammate direct channel" requirement.

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestTeammateSlash_NilRoster(t *testing.T) {
	out := runTeammateMessage(nil, "alice hi")
	if !strings.Contains(out, "no roster") {
		t.Errorf("nil roster should be reported; got %q", out)
	}
}

func TestTeammateSlash_Usage(t *testing.T) {
	roster := agent.NewRoster(10)
	cases := []string{"", "   ", "alice", "  alice  "}
	for _, arg := range cases {
		out := runTeammateMessage(roster, arg)
		if !strings.Contains(out, "usage:") {
			t.Errorf("arg=%q should print usage; got %q", arg, out)
		}
	}
}

func TestTeammateSlash_UnknownRecipient(t *testing.T) {
	roster := agent.NewRoster(10)
	out := runTeammateMessage(roster, "alice hi")
	if !strings.Contains(out, "no teammate") {
		t.Errorf("missing 'no teammate' hint; got %q", out)
	}
}

func TestTeammateSlash_DeliversToNamed(t *testing.T) {
	roster := agent.NewRoster(10)
	tm := &agent.Teammate{Name: "alice", AgentID: "agt-aaaa1111", Cancel: func() {}}
	if err := roster.Register(tm); err != nil {
		t.Fatalf("register: %v", err)
	}

	out := runTeammateMessage(roster, "alice please review the auth flow")
	if !strings.Contains(out, "message delivered to alice") {
		t.Errorf("expected delivery confirmation; got %q", out)
	}
	select {
	case msg := <-tm.Mailbox:
		if msg.From != "user" {
			t.Errorf("From = %q, want %q", msg.From, "user")
		}
		if !strings.Contains(msg.Body, "review the auth flow") {
			t.Errorf("Body lost the message text; got %q", msg.Body)
		}
	default:
		t.Errorf("mailbox didn't receive the PeerMessage")
	}
}

func TestTeammateSlash_RejectsAnonymousRecipient(t *testing.T) {
	roster := agent.NewRoster(10, 10)
	tm := &agent.Teammate{
		Name:      "_anon-deadbeef",
		AgentID:   "agt-12345678",
		Anonymous: true,
		Cancel:    func() {},
	}
	if err := roster.Register(tm); err != nil {
		t.Fatalf("register: %v", err)
	}
	if tm.Mailbox != nil {
		t.Fatalf("anonymous should have nil Mailbox; got %v", tm.Mailbox)
	}

	out := runTeammateMessage(roster, "_anon-deadbeef hi")
	if !strings.Contains(out, "anonymous sub-agent") {
		t.Errorf("expected anonymous rejection; got %q", out)
	}
}
