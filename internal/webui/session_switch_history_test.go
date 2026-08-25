package webui

import (
	"strings"
	"testing"
)

func TestDetachedRunningSessionReconcilesAuthoritativeHistory(t *testing.T) {
	chat, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	source := string(chat)
	for _, want := range []string{
		"let runningTurnNeedsHistorySync = false;",
		"function detachRunningTurnView()",
		"runningTurnNeedsHistorySync = true;",
		"async function syncViewedSessionHistory(sessionId)",
		"if (viewingTurn && runningTurnNeedsHistorySync)",
		"await syncViewedSessionHistory(resolvedTurnSessionId)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("chat.js missing detached-session history reconciliation contract %q", want)
		}
	}
}
