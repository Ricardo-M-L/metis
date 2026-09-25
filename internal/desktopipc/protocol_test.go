package desktopipc

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestEventRoundTripDropsLocalAuthorizationState(t *testing.T) {
	source := agent.Event{Kind: agent.EventPermissionRequest, PermissionTool: "Bash", PermissionInput: map[string]any{"api_key": "secret"}, PermissionReply: make(chan agent.PermissionDecision, 1), AskUserReply: make(chan string, 1), Err: errors.New("example"), SubAgentParentID: "parent", TraceCallID: "call", Elapsed: 123}
	event := FromEvent(source)
	var buffer bytes.Buffer
	if err := NewEncoder(&buffer).Encode(Message{Version: Version, Type: TypeEvent, ID: "permission-1", Event: &event}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buffer.String(), "secret") {
		t.Fatal("wire leaked permission secret")
	}
	message, err := NewDecoder(&buffer).Decode()
	if err != nil {
		t.Fatal(err)
	}
	restored := message.Event.ToEvent()
	if restored.PermissionReply != nil || restored.AskUserReply != nil {
		t.Fatal("wire restored a reply channel")
	}
	if _, ok := restored.PermissionPolicyInputForAuthorization(); ok {
		t.Fatal("wire restored authorization arguments")
	}
	if restored.Err == nil || restored.Err.Error() != "example" || restored.SubAgentParentID != "parent" || restored.Elapsed != 123 || restored.TraceCallID != "call" {
		t.Fatalf("lost event fields: %+v", restored)
	}
	if source.PermissionInput["api_key"] != "secret" {
		t.Fatal("wire conversion mutated source")
	}
}

func TestProtocolRejectsMalformedOrUnboundedMessages(t *testing.T) {
	for _, data := range []string{
		`{"version":2,"type":"reply","id":"x","decision":0}`,
		`{"version":1,"type":"reply","id":"x"}`,
		`{"version":1,"type":"reply","id":"x","decision":null}`,
		`{"version":1,"type":"reply","id":"x","decision":99}`,
		`{"version":1,"type":"reply","id":"x","decision":0,"unexpected":1}`,
		`{"version":1,"type":"event"}`,
		`{"version":1,"type":"reply","id":"x","decision":0} {}`,
		strings.Repeat("x", MaxMessageBytes+1),
	} {
		if _, err := NewDecoder(strings.NewReader(data + "\n")).Decode(); err == nil {
			t.Fatal("accepted malformed or oversized message")
		}
	}
	if _, err := NewDecoder(strings.NewReader("")).Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF=%v", err)
	}
	event := Event{Kind: agent.EventTextDelta, TextDelta: strings.Repeat("x", MaxMessageBytes)}
	var buffer bytes.Buffer
	if err := NewEncoder(&buffer).Encode(Message{Version: Version, Type: TypeEvent, Event: &event}); err == nil || buffer.Len() != 0 {
		t.Fatal("oversized event was partially emitted")
	}
}

func TestReplyExplicitlyEncodesAllow(t *testing.T) {
	var buffer bytes.Buffer
	if err := NewEncoder(&buffer).Encode(Message{Version: Version, Type: TypeReply, ID: "a", Decision: agent.PermissionDecisionAllow}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), `"decision":0`) {
		t.Fatal("allow decision was omitted")
	}
	if _, err := NewDecoder(&buffer).Decode(); err != nil {
		t.Fatal(err)
	}
}
