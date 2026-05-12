package agent

// peer_notify_test.go locks the recipient-side drain + inject flow
// for G.3 (2026-05-12). Counterpart to message_teammate.go's sender
// side: this proves that messages pushed onto a Loop's PeerInbox
// land in the message history wrapped as <peer_message> system
// reminders.

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestInjectPeerMessages_NilInboxIsNoOp — Loop.PeerInbox nil (the
// default for the root user loop) must not append anything. Without
// this guard, the top-level loop would crash on the nil channel.
func TestInjectPeerMessages_NilInboxIsNoOp(t *testing.T) {
	l := &Loop{}
	before := len(l.Messages)
	l.injectPeerMessages(nil)
	if len(l.Messages) != before {
		t.Errorf("nil PeerInbox should not modify Messages; before=%d after=%d", before, len(l.Messages))
	}
}

// TestInjectPeerMessages_EmptyDrainIsNoOp — Inbox wired but no
// pending messages → no synthetic user-message appended. Pin this
// so a busy-loop drain never spams a `<peer_message></peer_message>`
// noop wrapper into the prompt.
func TestInjectPeerMessages_EmptyDrainIsNoOp(t *testing.T) {
	ch := make(chan PeerMessage, 4)
	l := &Loop{PeerInbox: ch}
	before := len(l.Messages)
	l.injectPeerMessages(nil)
	if len(l.Messages) != before {
		t.Errorf("empty drain should not modify Messages; before=%d after=%d", before, len(l.Messages))
	}
}

// TestInjectPeerMessages_SingleMessage — one message in, one user
// message out wrapped in <peer_message> tags. Sender name + body
// must round-trip into the prompt body so the model can address it.
func TestInjectPeerMessages_SingleMessage(t *testing.T) {
	ch := make(chan PeerMessage, 4)
	ch <- PeerMessage{From: "alice", Body: "found the bug", Sent: time.Now()}
	l := &Loop{PeerInbox: ch}
	l.injectPeerMessages(nil)

	if len(l.Messages) != 1 {
		t.Fatalf("expected 1 injected message; got %d", len(l.Messages))
	}
	got := l.Messages[0]
	if got.Role != llm.RoleUser {
		t.Errorf("peer message must inject as user role; got %q", got.Role)
	}
	body := got.Content[0].Text
	if !strings.Contains(body, "<peer_message>") || !strings.Contains(body, "</peer_message>") {
		t.Errorf("body should wrap in <peer_message> tags; got %q", body)
	}
	if !strings.Contains(body, `Teammate "alice"`) {
		t.Errorf("body should name the sender; got %q", body)
	}
	if !strings.Contains(body, "found the bug") {
		t.Errorf("body should include the message text; got %q", body)
	}
}

// TestInjectPeerMessages_MultipleMessagesCollapse — 3 messages drain
// into 1 user message with a count + per-sender separators. Critical
// so a flood of peer messages doesn't fragment the prompt.
func TestInjectPeerMessages_MultipleMessagesCollapse(t *testing.T) {
	ch := make(chan PeerMessage, 4)
	ch <- PeerMessage{From: "alice", Body: "msg-1", Sent: time.Now()}
	ch <- PeerMessage{From: "bob", Body: "msg-2", Sent: time.Now()}
	ch <- PeerMessage{From: "carol", Body: "msg-3", Sent: time.Now()}
	l := &Loop{PeerInbox: ch}
	l.injectPeerMessages(nil)

	if len(l.Messages) != 1 {
		t.Fatalf("3 peer messages should collapse to 1 user message; got %d", len(l.Messages))
	}
	body := l.Messages[0].Content[0].Text
	if !strings.Contains(body, "3 teammates sent you messages") {
		t.Errorf("body should announce the count; got %q", body)
	}
	for _, who := range []string{"alice", "bob", "carol"} {
		if !strings.Contains(body, who) {
			t.Errorf("body should include sender %q; got %q", who, body)
		}
	}
	for _, want := range []string{"msg-1", "msg-2", "msg-3"} {
		if !strings.Contains(body, want) {
			t.Errorf("body should include payload %q; got %q", want, body)
		}
	}
}
