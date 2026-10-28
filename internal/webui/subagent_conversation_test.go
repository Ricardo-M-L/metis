package webui

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func newWebConversationTranscript(t *testing.T, dir, id, parent string, messages []llm.Message) *agent.SubAgentTranscript {
	t.Helper()
	transcript, err := agent.NewSubAgentTranscript(dir, id, agent.NewSubAgentHeader(id, "fixture", parent, id, "", "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.Checkpoint(messages); err != nil {
		t.Fatal(err)
	}
	return transcript
}

func conversationHTTP(t *testing.T, s *Server, path string) (int, subAgentConversationView, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	var body subAgentConversationView
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return rr.Code, body, rr.Body.String()
}

func webConversationText(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func TestSubAgentConversationAPIReopensFullHistoryWithPresentationBoundary(t *testing.T) {
	_, store := testServer(t)
	longResult := "BEGIN-" + strings.Repeat("full tool output\n", 24000) + "-END"
	thinking := "thinking-" + strings.Repeat("analysis details\n", 1600)
	messages := []llm.Message{
		webConversationText(llm.RoleSystem, "private system prompt"),
		webConversationText(llm.RoleUser, "Read the complete output.<system-reminder>PRIVATE INTERNAL</system-reminder>"),
		webConversationText(llm.RoleUser, "<peer_message>PRIVATE PEER</peer_message>"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "thinking", Text: thinking},
			{Type: "tool_use", ToolUseID: "call-1", ToolName: "Bash", ToolInput: map[string]any{"command": "read fixture", "api_key": "fixture-credential"}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "call-1", ToolResult: longResult, IsError: true}}},
		webConversationText(llm.RoleAssistant, "## Result\n\nDone."),
	}
	tr := newWebConversationTranscript(t, store.Dir, "agt-history", "parent", messages)
	if err := tr.AppendTerminal(agent.SubAgentTerminal{Status: "failed", EndedAt: time.Now(), ExitError: "fixture tool failure"}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	// This server has no live roster or worker state.
	restarted := NewServer("127.0.0.1:0", nil, store)
	code, body, raw := conversationHTTP(t, restarted, "/api/subagents/agt-history/conversation?sessionId=parent")
	if code != http.StatusOK {
		t.Fatalf("conversation=%d %s", code, raw)
	}
	if body.Agent.Status != "failed" || body.Agent.ExitError != "fixture tool failure" || body.Agent.Task != "Read the complete output." {
		t.Fatalf("wrong historical detail: %+v", body.Agent)
	}
	if body.TotalMessages != 4 || body.StartIndex != 0 || body.HasMore || body.Revision == "" || body.Revision != body.Agent.ConversationRevision {
		t.Fatalf("wrong history metadata: %+v", body)
	}
	if got := body.Messages[1].Content[0].Text; got != thinking {
		t.Fatalf("thinking truncated: got %d want %d bytes", len(got), len(thinking))
	}
	if got := body.Messages[2].Content[0]; got.ToolResult != longResult || !got.IsError {
		t.Fatalf("full tool output/error lost: got %d want %d bytes error=%v", len(got.ToolResult), len(longResult), got.IsError)
	}
	for _, hidden := range []string{"private system prompt", "PRIVATE INTERNAL", "PRIVATE PEER", "fixture-credential"} {
		if strings.Contains(raw, hidden) {
			t.Fatalf("presentation leaked %q", hidden)
		}
	}
	if body.Usage.Available || body.Usage.TotalTokens != nil || !strings.Contains(raw, `"totalTokens":null`) {
		t.Fatalf("missing usage must remain unknown: %+v", body.Usage)
	}
	if body.Completeness.State != "recorded" || body.Completeness.LiveTailAvailable {
		t.Fatalf("wrong completeness: %+v", body.Completeness)
	}
	if messages[3].Content[1].ToolInput["api_key"] != "fixture-credential" {
		t.Fatal("presentation mutated source fixture")
	}
}

func TestSubAgentConversationAPIPaginatesRepeatedMessagesAndResetSegments(t *testing.T) {
	s, store := testServer(t)
	repeated := webConversationText(llm.RoleAssistant, "same answer")
	tr := newWebConversationTranscript(t, store.Dir, "agt-pages", "parent", []llm.Message{webConversationText(llm.RoleUser, "task"), repeated, repeated})
	if err := tr.Checkpoint([]llm.Message{webConversationText(llm.RoleUser, "compacted context"), repeated}); err != nil {
		t.Fatal(err)
	}
	read := func(suffix string) subAgentConversationView {
		t.Helper()
		code, body, raw := conversationHTTP(t, s, "/api/subagents/agt-pages/conversation?sessionId=parent&limit=2"+suffix)
		if code != http.StatusOK {
			t.Fatalf("read=%d %s", code, raw)
		}
		return body
	}
	last := read("")
	if last.StartIndex != 3 || last.TotalMessages != 5 || !last.HasMore || last.NextBefore == nil || *last.NextBefore != 3 || len(last.Messages) != 2 {
		t.Fatalf("latest page: %+v", last)
	}
	middle, first := read("&before=3"), read("&before=1")
	if middle.StartIndex != 1 || len(middle.Messages) != 2 || !reflect.DeepEqual(middle.Messages[0], middle.Messages[1]) || first.HasMore || first.NextBefore != nil || first.StartIndex != 0 {
		t.Fatalf("repeated messages or page boundary lost: middle=%+v first=%+v", middle, first)
	}
	if last.Completeness.State != "context_reset" || last.Completeness.ContextResets != 1 || len(last.Segments) != 2 || last.Segments[1].StartIndex != 3 {
		t.Fatalf("reset segment missing: %+v %+v", last.Completeness, last.Segments)
	}
	empty := read("&before=0")
	if len(empty.Messages) != 0 || empty.StartIndex != 0 || empty.HasMore {
		t.Fatalf("empty first boundary: %+v", empty)
	}
	for _, suffix := range []string{"&before=-1", "&before=6", "&before=bad", "&limit=0", "&limit=201"} {
		code, _, _ := conversationHTTP(t, s, "/api/subagents/agt-pages/conversation?sessionId=parent"+suffix)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid pagination %s status=%d", suffix, code)
		}
	}
}

func TestSubAgentConversationAPIOwnershipValidationAndUnrecordedWorker(t *testing.T) {
	s, store := testServer(t)
	newWebConversationTranscript(t, store.Dir, "agt-owner", "parent", []llm.Message{webConversationText(llm.RoleUser, "task")})
	// Even a stale worker snapshot must not override a replaced disk owner.
	s.setWorkerSnapshot("foreign", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-owner", Status: "running"}}})
	s.setWorkerSnapshot("parent", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-new", Status: "queued"}}})
	for _, path := range []string{
		"/api/subagents/agt-owner/conversation?sessionId=foreign",
		"/api/subagents/agt-owner?sessionId=foreign",
		"/api/subagents/agt-owner/events?sessionId=foreign",
		"/api/subagents/agt-new/conversation?sessionId=foreign",
	} {
		code, _, _ := conversationHTTP(t, s, path)
		if code != http.StatusNotFound {
			t.Fatalf("foreign ownership %s status=%d", path, code)
		}
	}
	for _, query := range []string{"", "?sessionId=..", "?sessionId=" + url.QueryEscape("bad\x00parent"), "?sessionId=a%2Fb"} {
		code, _, _ := conversationHTTP(t, s, "/api/subagents/agt-owner/conversation"+query)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid parent %s status=%d", query, code)
		}
	}
	for _, id := range []string{"bad%5Cagent", "bad%00agent", "%20agent"} {
		code, _, _ := conversationHTTP(t, s, "/api/subagents/"+id+"/conversation?sessionId=parent")
		if code != http.StatusBadRequest {
			t.Fatalf("invalid agent %q status=%d", id, code)
		}
	}
	code, body, raw := conversationHTTP(t, s, "/api/subagents/agt-new/conversation?sessionId=parent")
	if code != http.StatusOK || body.Agent.Status != "queued" || len(body.Messages) != 0 || body.Completeness.State != "unavailable" || body.Usage.Available {
		t.Fatalf("unrecorded worker must be explicit: %d %s", code, raw)
	}
}

type subAgentDirectoryTestView struct {
	SessionID  string               `json:"sessionId"`
	Agents     []subAgentDetailView `json:"agents"`
	HasMore    bool                 `json:"hasMore"`
	NextCursor *string              `json:"nextCursor"`
}

func readSubAgentDirectory(t *testing.T, s *Server, path string) subAgentDirectoryTestView {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	var body subAgentDirectoryTestView
	if rr.Code != http.StatusOK {
		t.Fatalf("directory=%d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSubAgentDirectoryAPIRestartAndLivePagination(t *testing.T) {
	_, store := testServer(t)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("agt-disk-%d", i)
		tr := newWebConversationTranscript(t, store.Dir, id, "parent", []llm.Message{webConversationText(llm.RoleUser, "complete task "+id)})
		if err := tr.AppendTerminal(agent.SubAgentTerminal{Status: "completed", EndedAt: time.Now(), Result: strings.Repeat("large preview", 1000)}); err != nil {
			t.Fatal(err)
		}
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
	}
	newWebConversationTranscript(t, store.Dir, "agt-foreign", "foreign", nil)
	restarted := NewServer("127.0.0.1:0", nil, store)
	restarted.setWorkerSnapshot("parent", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-live", Status: "running"}, {AgentID: "agt-disk-0", Status: "queued"}}})
	restarted.recordWorkerSubAgentLifecycle("parent", agent.Event{Kind: agent.EventSubAgentStart, SubAgentID: "agt-start", SubAgentStatus: "queued"})
	seen := make(map[string]subAgentDetailView)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		body := readSubAgentDirectory(t, restarted, "/api/subagents?sessionId=parent&limit=2&cursor="+cursor)
		if body.SessionID != "parent" || len(body.Agents) > 2 {
			t.Fatalf("wrong page bound: %+v", body)
		}
		for _, child := range body.Agents {
			if _, duplicate := seen[child.AgentID]; duplicate {
				t.Fatalf("duplicate directory identity: %s", child.AgentID)
			}
			if child.AgentID == "agt-foreign" || child.Output != "" || child.Result != "" {
				t.Fatalf("directory leaked owner or full preview: %+v", child)
			}
			seen[child.AgentID] = child
		}
		if !body.HasMore {
			if body.NextCursor != nil {
				t.Fatal("terminal page retained cursor")
			}
			break
		}
		if body.NextCursor == nil || *body.NextCursor == cursor {
			t.Fatalf("cursor did not advance: %+v", body)
		}
		cursor = *body.NextCursor
	}
	if len(seen) != 6 || seen["agt-disk-0"].Status != "queued" || seen["agt-live"].Status != "running" || seen["agt-start"].Status != "queued" || seen["agt-disk-3"].Status != "completed" || seen["agt-disk-3"].Task != "complete task agt-disk-3" {
		t.Fatalf("missing persisted/live directory: %+v", seen)
	}
	for _, path := range []string{"/api/subagents", "/api/subagents?sessionId=..", "/api/subagents?sessionId=parent&limit=0", "/api/subagents?sessionId=parent&limit=101", "/api/subagents?sessionId=parent&cursor=-1", "/api/subagents?sessionId=parent&cursor=999"} {
		rr := httptest.NewRecorder()
		restarted.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid directory %s status=%d", path, rr.Code)
		}
	}
}

func TestSubAgentDirectoryAPIBoundsHeaderScanning(t *testing.T) {
	s, store := testServer(t)
	dir := filepath.Join(store.Dir, agent.SubAgentTranscriptDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < subAgentDirectoryScanLimit; i++ {
		id := fmt.Sprintf("agt-a%04d", i)
		header := agent.NewSubAgentHeader(id, "fixture", "foreign", "", "", "default")
		data, _ := json.Marshal(map[string]any{"type": "header", "header": header})
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newWebConversationTranscript(t, store.Dir, "agt-zzz", "parent", []llm.Message{webConversationText(llm.RoleUser, "task")})
	first := readSubAgentDirectory(t, s, "/api/subagents?sessionId=parent&limit=1")
	if len(first.Agents) != 0 || !first.HasMore || first.NextCursor == nil {
		t.Fatalf("header scan exceeded bound: %+v", first)
	}
	next := readSubAgentDirectory(t, s, "/api/subagents?sessionId=parent&limit=1&cursor="+*first.NextCursor)
	if len(next.Agents) != 1 || next.Agents[0].AgentID != "agt-zzz" || next.HasMore {
		t.Fatalf("scan continuation lost parent entry: %+v", next)
	}
}

func TestSubAgentDirectoryAPIKeysetSurvivesLiveRosterChanges(t *testing.T) {
	s, store := testServer(t)
	for _, id := range []string{"agt-a", "agt-b", "agt-c"} {
		newWebConversationTranscript(t, store.Dir, id, "parent", []llm.Message{webConversationText(llm.RoleUser, "task "+id)})
	}
	s.setWorkerSnapshot("parent", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-b", Status: "running"}}})
	first := readSubAgentDirectory(t, s, "/api/subagents?sessionId=parent&limit=1")
	if len(first.Agents) != 1 || first.Agents[0].AgentID != "agt-a" || first.NextCursor == nil {
		t.Fatalf("wrong initial keyset: %+v", first)
	}
	// The old offset-union scheme reordered existing entries at this point.
	s.setWorkerSnapshot("parent", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-c", Status: "running"}, {AgentID: "agt-0-new", Status: "queued"}}})
	next := readSubAgentDirectory(t, s, "/api/subagents?sessionId=parent&limit=1&cursor="+url.QueryEscape(*first.NextCursor))
	if len(next.Agents) != 1 || next.Agents[0].AgentID != "agt-b" || next.NextCursor == nil {
		t.Fatalf("live roster movement skipped/repeated a historical child: %+v", next)
	}
	last := readSubAgentDirectory(t, s, "/api/subagents?sessionId=parent&limit=1&cursor="+url.QueryEscape(*next.NextCursor))
	if len(last.Agents) != 1 || last.Agents[0].AgentID != "agt-c" || last.HasMore {
		t.Fatalf("keyset continuation lost terminal entry: %+v", last)
	}
}

func TestSubAgentConversationStreamEmitsPhaseSlotAndRevisionWithoutOutput(t *testing.T) {
	s, store := testServer(t)
	tr := newWebConversationTranscript(t, store.Dir, "agt-stream", "parent", []llm.Message{webConversationText(llm.RoleUser, "task")})
	activity, err := agent.NewSubAgentActivityRecorder(store.Dir, "agt-stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = activity.Close() })
	status := desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-stream", Status: "running", ExecutionPhase: "waiting_children"}}}
	s.setWorkerSnapshot("parent", status)
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/subagents/agt-stream/events?sessionId=parent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	readSnapshot := func() subAgentDetailView {
		t.Helper()
		event, payload := nextSubAgentSSE(t, reader)
		var body struct {
			Agent subAgentDetailView `json:"agent"`
		}
		if event != "snapshot" {
			t.Fatalf("event=%q payload=%v", event, payload)
		}
		encoded, _ := json.Marshal(payload)
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		return body.Agent
	}
	initial := readSnapshot()
	status.Agents = append([]desktopipc.Subagent(nil), status.Agents...)
	status.Agents[0].ExecutionPhase = "waiting_background"
	s.setWorkerSnapshot("parent", status)
	if got := readSnapshot(); got.ExecutionPhase != "waiting_background" || got.Output != "" {
		t.Fatalf("phase-only event missing: %+v", got)
	}
	status.Agents = append([]desktopipc.Subagent(nil), status.Agents...)
	status.Agents[0].HoldsExecutionSlot = true
	s.setWorkerSnapshot("parent", status)
	if got := readSnapshot(); !got.HoldsExecutionSlot {
		t.Fatalf("slot-only event missing: %+v", got)
	}
	if err := tr.Checkpoint([]llm.Message{webConversationText(llm.RoleUser, "task"), webConversationText(llm.RoleAssistant, "complete message")}); err != nil {
		t.Fatal(err)
	}
	if got := readSnapshot(); got.ConversationRevision == initial.ConversationRevision || got.Output != "" {
		t.Fatalf("checkpoint-only invalidation missing: %+v", got)
	}
	if err := activity.Observe(agent.Event{Kind: agent.EventToolStart, TraceCallID: "activity-stream", ToolUseID: "provider-stream", ToolName: "Read", ToolInput: map[string]any{"path": "fixture"}}); err != nil {
		t.Fatal(err)
	}
	toolStart := readSnapshot()
	if toolStart.Output != "" {
		t.Fatal("tool boundary was incorrectly spliced into text output")
	}
	if err := activity.Observe(agent.Event{Kind: agent.EventToolResult, TraceCallID: "activity-stream", ToolUseID: "provider-stream", ToolName: "Read", ToolResult: &agent.ToolResult{Output: "full live result"}}); err != nil {
		t.Fatal(err)
	}
	if got := readSnapshot(); got.ConversationRevision == toolStart.ConversationRevision || got.Output != "" {
		t.Fatalf("activity-only invalidation missing: %+v", got)
	}
	status.Agents = append([]desktopipc.Subagent(nil), status.Agents...)
	status.Agents[0].Status = "completed"
	s.setWorkerSnapshot("parent", status)
	event, _ := nextSubAgentSSE(t, reader)
	if event != "terminal" {
		t.Fatalf("terminal event=%q", event)
	}
}

func TestSubAgentConversationAPILiveToolsFullOutputAndOccurrenceReconciliation(t *testing.T) {
	s, store := testServer(t)
	task := webConversationText(llm.RoleUser, "Read full tool output")
	tr := newWebConversationTranscript(t, store.Dir, "agt-tools", "parent", []llm.Message{task})
	recorder, err := agent.NewSubAgentActivityRecorder(store.Dir, "agt-tools")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	argument, output := "ARG-"+strings.Repeat("input\n", 5000), "OUT-"+strings.Repeat("result\n", 30000)
	for _, event := range []agent.Event{
		{Kind: agent.EventToolStart, TraceCallID: "first-occurrence", ToolUseID: "provider-reused", ToolName: "Bash", ToolInput: map[string]any{"command": argument, "api_key": "secret-fixture"}},
		{Kind: agent.EventToolResult, TraceCallID: "first-occurrence", ToolUseID: "provider-reused", ToolName: "Bash", ToolResult: &agent.ToolResult{Output: output, IsError: true}},
		{Kind: agent.EventToolStart, TraceCallID: "second-occurrence", ToolUseID: "provider-reused", ToolName: "Bash", ToolInput: map[string]any{"command": "second task"}},
	} {
		if err := recorder.Observe(event); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/subagents/agt-tools/conversation?sessionId=parent&before=0"
	code, before, raw := conversationHTTP(t, s, path)
	if code != http.StatusOK || !before.Activity.Available || before.Activity.Partial || !before.Completeness.LiveTailAvailable || len(before.LiveTools) != 2 {
		t.Fatalf("tool before checkpoint missing: code=%d activity=%+v live=%d", code, before.Activity, len(before.LiveTools))
	}
	if before.LiveTools[0].Input["command"] != argument || before.LiveTools[0].Result != output || before.LiveTools[0].Status != "error" || !before.LiveTools[0].IsError || before.LiveTools[1].Status != "running" {
		t.Fatal("live arguments, full output, or status was changed/truncated")
	}
	if strings.Contains(raw, "secret-fixture") {
		t.Fatal("live tool input credential escaped presentation boundary")
	}
	history := []llm.Message{task,
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "provider-reused", TraceCallID: "first-occurrence", ToolName: "Bash"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "provider-reused", TraceCallID: "first-occurrence", ToolResult: output, IsError: true}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "provider-reused", TraceCallID: "second-occurrence", ToolName: "Bash"}}},
		// Legacy results with a reused provider ID cannot prove occurrence.
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "provider-reused", ToolResult: "legacy result"}}},
	}
	if err := tr.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	code, after, _ := conversationHTTP(t, s, path)
	if code != http.StatusOK || len(after.Messages) != 0 || len(after.LiveTools) != 1 || after.LiveTools[0].TraceCallID != "second-occurrence" || after.LiveTools[0].ID != before.LiveTools[1].ID || after.Revision == before.Revision {
		t.Fatalf("exact occurrence reconciliation failed across pagination: code=%d tools=%+v", code, after.LiveTools)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	_, closed, _ := conversationHTTP(t, s, path)
	if !closed.Activity.Partial || len(closed.LiveTools) != 1 || closed.LiveTools[0].Status != "incomplete" {
		t.Fatalf("pending call was presented as successful after close: %+v", closed.Activity)
	}
}

func TestSubAgentConversationAPIUsageUsesReceiptBucketsAndPreservesZero(t *testing.T) {
	s, store := testServer(t)
	newWebConversationTranscript(t, store.Dir, "agt-usage-api", "parent", []llm.Message{webConversationText(llm.RoleUser, "task")})
	header, err := agent.LoadSubAgentHeader(store.Dir, "agt-usage-api")
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader, _ := json.Marshal(header)
	digest := sha256.Sum256(encodedHeader)
	path := filepath.Join(store.Dir, "subagent-usage", "agt-usage-api.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][4]int64{{12, 3, 8, 2}, {0, 0, 0, 0}} {
		record := map[string]any{"version": 1, "agent_id": header.ID, "parent_session_id": header.SubAgentOf, "header_sha256": fmt.Sprintf("%x", digest), "receipt_id": "stable-receipt", "reported": true,
			"input_tokens": values[0], "output_tokens": values[1], "cache_read_input_tokens": values[2], "cache_creation_input_tokens": values[3]}
		encoded, _ := json.Marshal(record)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.Write(append(encoded, '\n'))
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("write usage fixture: %v %v", writeErr, closeErr)
		}
		code, body, raw := conversationHTTP(t, s, "/api/subagents/agt-usage-api/conversation?sessionId=parent")
		if code != http.StatusOK || !body.Usage.Available || body.Usage.TotalTokens == nil || *body.Usage.TotalTokens != values[0]+values[1]+values[2]+values[3] || *body.Usage.InputTokens != values[0] || *body.Usage.CacheReadInputTokens != values[2] {
			t.Fatalf("wrong receipt accounting: code=%d raw=%s", code, raw)
		}
	}
}
