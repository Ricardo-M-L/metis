package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceEndpointCountsToolErrorsAcrossAgentsAndPages(t *testing.T) {
	for _, tc := range []struct {
		name          string
		timingErrors  int
		terminalError bool
		wantErrors    int
	}{
		{name: "root_and_child_errors", wantErrors: 2},
		{name: "overlapping_timing_is_not_added", timingErrors: 1, wantErrors: 2},
		{name: "timing_remains_a_lower_bound", timingErrors: 3, wantErrors: 3},
		{name: "terminal_errors_count_once", terminalError: true, wantErrors: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldAdapter := rtpkg.CurrentTraceAdapter()
			defer rtpkg.SetTraceAdapter(oldAdapter)
			if adapter := rtpkg.InstallTrace(t.TempDir()); adapter == nil {
				t.Fatal("InstallTrace returned nil adapter")
			}
			traceStore := rtpkg.CurrentTraceStore()
			defer traceStore.Close()

			const sessionID = "trace-error-stats"
			events := []session.TraceEvent{
				{Kind: "tool_start", ToolName: "Read", ToolUseID: "root-read", TraceInvocationID: "root", TraceCallID: "read-call"},
				{Kind: "tool_result", ToolName: "Read", ToolUseID: "root-read", TraceInvocationID: "root", TraceCallID: "read-call", IsError: true},
				{Kind: "tool_start", ToolName: "Agent", ToolUseID: "agent", TraceInvocationID: "child", TraceParentInvocationID: "root", TraceCallID: "agent-call"},
				{Kind: "tool_start", ToolName: "Grep", ToolUseID: "child-grep", ParentID: "agent", TraceInvocationID: "child", TraceCallID: "grep-call"},
				{Kind: "tool_result", ToolName: "Grep", ToolUseID: "child-grep", ParentID: "child-grep", TraceInvocationID: "child", TraceCallID: "grep-call", IsError: true},
				{Kind: "tool_result", ToolName: "Agent", ToolUseID: "agent", TraceInvocationID: "child", TraceParentInvocationID: "root", TraceCallID: "agent-call"},
			}
			if tc.terminalError {
				// Error events can carry IsError too; that is still one error.
				events = append(events,
					session.TraceEvent{Kind: "error", IsError: true},
					session.TraceEvent{Kind: "error"},
				)
			}
			base := time.Date(2026, time.September, 28, 10, 0, 0, 0, time.UTC)
			for i, event := range events {
				event.SessionID = sessionID
				event.Turn = 1
				event.TS = base.Add(time.Duration(i) * time.Second)
				if err := traceStore.Append(&event); err != nil {
					t.Fatal(err)
				}
			}

			s, sessionStore := testServer(t)
			if tc.timingErrors > 0 {
				recorder := sessionStore.NewTimingRecorder(sessionID)
				for i := 0; i < tc.timingErrors; i++ {
					recorder.Record("Read", time.Millisecond, true)
				}
				steps, err := sessionStore.ReadTiming(sessionID)
				if err != nil || len(steps) != tc.timingErrors {
					t.Fatalf("timing fixture: steps=%d err=%v", len(steps), err)
				}
			}

			// Every page must report the full trace's errors, including failures
			// outside that page and below a successfully completed Agent call.
			cursor := ""
			for page := 0; ; page++ {
				url := "/api/trace?sessionId=" + sessionID + "&limit=2"
				if cursor != "" {
					url += "&cursor=" + cursor
				}
				rr := httptest.NewRecorder()
				s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("page %d: status=%d body=%s", page, rr.Code, rr.Body.String())
				}
				var payload struct {
					Stats      traceStats `json:"stats"`
					HasMore    bool       `json:"hasMore"`
					NextCursor string     `json:"nextCursor"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Stats.Errors != tc.wantErrors {
					t.Errorf("page %d: errors=%d, want %d", page, payload.Stats.Errors, tc.wantErrors)
				}
				if payload.Stats.ToolCalls != 3 {
					t.Errorf("page %d: toolCalls=%d, want 3", page, payload.Stats.ToolCalls)
				}
				if !payload.HasMore {
					break
				}
				if payload.NextCursor == "" || payload.NextCursor == cursor || page >= len(events) {
					t.Fatalf("page %d: cursor did not advance: %q", page, payload.NextCursor)
				}
				cursor = payload.NextCursor
			}
		})
	}
}
