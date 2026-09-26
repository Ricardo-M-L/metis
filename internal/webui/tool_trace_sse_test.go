package webui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestToolSSEUsesOccurrenceIdentityWhenProviderIDRepeats(t *testing.T) {
	const providerID = "provider-reused-id"
	calls := []struct {
		kind    agent.EventKind
		traceID string
	}{
		{agent.EventToolArgsDelta, "trace-call-1"},
		{agent.EventToolStart, "trace-call-1"},
		{agent.EventToolResult, "trace-call-1"},
		{agent.EventToolStart, "trace-call-2"},
		{agent.EventToolResult, "trace-call-2"},
	}
	for i, call := range calls {
		recorder := httptest.NewRecorder()
		(&Server{}).writeHubEvent(recorder, hubEvent{
			sequence: uint64(i + 1),
			session:  "session-1",
			ev: agent.Event{
				Kind:        call.kind,
				ToolName:    "Bash",
				ToolUseID:   providerID,
				TraceCallID: call.traceID,
			},
		})
		var payload map[string]any
		for _, line := range strings.Split(recorder.Body.String(), "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatal(err)
				}
			}
		}
		if payload["id"] != providerID || payload["traceCallId"] != call.traceID {
			t.Fatalf("event %d (%v): payload = %#v, want provider ID %q and occurrence ID %q", i, call.kind, payload, providerID, call.traceID)
		}
	}
}

func TestToolSSEOmitAbsentOccurrenceIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Server{}).writeHubEvent(recorder, hubEvent{
		sequence: 1,
		session:  "session-1",
		ev: agent.Event{
			Kind:      agent.EventToolStart,
			ToolName:  "Bash",
			ToolUseID: "older-event",
		},
	})
	if strings.Contains(recorder.Body.String(), "traceCallId") {
		t.Fatalf("absent occurrence ID should remain omitted: %s", recorder.Body.String())
	}
}
