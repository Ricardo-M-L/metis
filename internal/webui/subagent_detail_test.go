package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestSubAgentDetailServesLiveAndRetainedOutput(t *testing.T) {
	_, store := testServer(t)
	roster := agent.NewRoster(2)
	teammate := &agent.Teammate{
		Name:       "transport",
		AgentID:    "agt-transport",
		Background: true,
		Started:    time.Now().Add(-2 * time.Second),
	}
	teammate.AppendText("reading the runtime\n")
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	transcript, err := agent.NewSubAgentTranscript(store.Dir, teammate.AgentID,
		agent.NewSubAgentHeader(teammate.AgentID, "fixture-model", "session-a", teammate.Name, t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})

	read := func(method string) (int, http.Header, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		server.handler().ServeHTTP(rr, httptest.NewRequest(method, "/api/subagents/agt-transport?sessionId=session-a", nil))
		body := map[string]any{}
		if rr.Body.Len() > 0 {
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v\n%s", err, rr.Body.String())
			}
		}
		return rr.Code, rr.Header(), body
	}

	code, _, payload := read(http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d: %#v", code, payload)
	}
	view, _ := payload["agent"].(map[string]any)
	if got := view["name"]; got != "transport" {
		t.Fatalf("name = %#v", got)
	}
	if got := view["status"]; got != "running" {
		t.Fatalf("status = %#v", got)
	}
	if got := view["output"]; got != "reading the runtime\n" {
		t.Fatalf("live output = %#v", got)
	}
	if got := view["background"]; got != true {
		t.Fatalf("background = %#v", got)
	}
	if got := view["sessionId"]; got != "session-a" {
		t.Fatalf("session ownership = %#v", got)
	}

	// The roster keeps completed entries briefly specifically so the Desktop
	// can still open an agent whose work finished while its status menu was up.
	teammate.Finish(agent.StatusFailed, "partial final result", errors.New("provider timeout"), "timeout 30s")
	roster.UnregisterTeammate(teammate)
	code, _, payload = read(http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("GET retained detail = %d: %#v", code, payload)
	}
	view, _ = payload["agent"].(map[string]any)
	for key, want := range map[string]string{
		"status":    "failed",
		"result":    "partial final result",
		"stopHint":  "timeout 30s",
		"exitError": "provider timeout",
	} {
		if got := view[key]; got != want {
			t.Fatalf("retained %s = %#v, want %q", key, got, want)
		}
	}

	code, headers, _ := read(http.MethodPost)
	if code != http.StatusMethodNotAllowed || headers.Get("Allow") != http.MethodGet {
		t.Fatalf("POST detail = %d Allow=%q, want 405 GET", code, headers.Get("Allow"))
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/subagents/agt-transport", http.StatusBadRequest},
		{"/api/subagents/agt-transport?sessionId=session-b", http.StatusNotFound},
		{"/api/subagents/agt-transport/events?sessionId=session-b", http.StatusNotFound},
	} {
		rr := httptest.NewRecorder()
		server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != tc.want {
			t.Fatalf("GET %s = %d, want %d", tc.path, rr.Code, tc.want)
		}
	}
}

func TestSubAgentDetailOutputBoundPreservesHeadAndTail(t *testing.T) {
	value := "开头-" + strings.Repeat("x", subAgentDetailOutputLimit+20) + "-结尾"
	got, truncated := trimSubAgentDetailOutput(value)
	if !truncated {
		t.Fatal("large output was not marked truncated")
	}
	if !strings.HasPrefix(got, "开头-") || !strings.HasSuffix(got, "-结尾") {
		t.Fatalf("bounded output did not preserve both ends: %q…%q", got[:min(20, len(got))], got[max(0, len(got)-20):])
	}
	if !strings.Contains(got, "METIS truncated this sub-agent output") {
		t.Fatalf("bounded output has no truncation marker: %q", got)
	}
}

func TestSubAgentDetailReopensFromTranscriptAfterRestart(t *testing.T) {
	_, store := testServer(t)
	agentID := "agt-persisted"
	transcript, err := agent.NewSubAgentTranscript(store.Dir, agentID,
		agent.NewSubAgentHeader(agentID, "fixture-model", "session-a", "auditor", t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	ended := time.Now()
	if err := transcript.AppendTerminal(agent.SubAgentTerminal{
		Status: "completed", Background: true, EndedAt: ended,
		Output: "checked files", Result: "audit complete", StopHint: "end_turn",
	}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	// No roster or worker snapshot exists in this new Server instance.
	restarted := NewServer("127.0.0.1:0", nil, store)
	for _, tc := range []struct {
		session string
		want    int
	}{
		{"session-a", http.StatusOK},
		{"session-b", http.StatusNotFound},
	} {
		rr := httptest.NewRecorder()
		path := "/api/subagents/" + agentID + "?sessionId=" + tc.session
		restarted.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != tc.want {
			t.Fatalf("GET %s = %d, want %d: %s", path, rr.Code, tc.want, rr.Body.String())
		}
		if tc.want == http.StatusOK {
			var body struct {
				Agent subAgentDetailView `json:"agent"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Agent.SessionID != "session-a" || body.Agent.Status != "completed" || body.Agent.Result != "audit complete" || !body.Agent.Background {
				t.Fatalf("restart detail = %+v", body.Agent)
			}
		}
	}
}

func TestSubAgentDetailStreamEmitsSnapshotDeltaAndTerminal(t *testing.T) {
	_, store := testServer(t)
	roster := agent.NewRoster(2)
	teammate := &agent.Teammate{
		Name:       "transport",
		AgentID:    "agt-transport",
		Background: true,
		Started:    time.Now().Add(-time.Second),
	}
	teammate.AppendText("initial ")
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	transcript, err := agent.NewSubAgentTranscript(store.Dir, teammate.AgentID,
		agent.NewSubAgentHeader(teammate.AgentID, "fixture-model", "session-a", teammate.Name, t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/subagents/agt-transport/events?sessionId=session-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("stream Content-Type = %q", got)
	}
	reader := bufio.NewReader(resp.Body)

	event, payload := nextSubAgentSSE(t, reader)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	view, _ := payload["agent"].(map[string]any)
	if got := view["output"]; got != "initial " {
		t.Fatalf("snapshot output = %#v", got)
	}

	teammate.AppendText("live text")
	event, payload = nextSubAgentSSE(t, reader)
	if event != "delta" || payload["delta"] != "live text" {
		t.Fatalf("live SSE = %q %#v, want delta", event, payload)
	}

	teammate.Finish(agent.StatusCompleted, "initial live text", nil, "end_turn")
	event, payload = nextSubAgentSSE(t, reader)
	if event != "terminal" {
		t.Fatalf("terminal event = %q, want terminal", event)
	}
	view, _ = payload["agent"].(map[string]any)
	if got := view["status"]; got != "completed" {
		t.Fatalf("terminal status = %#v", got)
	}
	if got := view["result"]; got != "initial live text" {
		t.Fatalf("terminal result = %#v", got)
	}
}

func TestSubAgentDetailStreamRefreshesTruncatedTail(t *testing.T) {
	_, store := testServer(t)
	roster := agent.NewRoster(2)
	teammate := &agent.Teammate{
		Name: "transport", AgentID: "agt-long-output",
		Started: time.Now().Add(-time.Second),
	}
	teammate.AppendText(strings.Repeat("x", subAgentDetailOutputLimit+100))
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	transcript, err := agent.NewSubAgentTranscript(store.Dir, teammate.AgentID,
		agent.NewSubAgentHeader(teammate.AgentID, "fixture-model", "session-a", teammate.Name, t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpServer.URL+"/api/subagents/agt-long-output/events?sessionId=session-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	event, payload := nextSubAgentSSE(t, reader)
	initial, _ := payload["agent"].(map[string]any)
	if event != "snapshot" || initial["outputTruncated"] != true {
		t.Fatalf("initial SSE = %q %#v, want truncated snapshot", event, initial)
	}

	teammate.AppendText("-new-tail-1")
	event, payload = nextSubAgentSSE(t, reader)
	updated, _ := payload["agent"].(map[string]any)
	output, _ := updated["output"].(string)
	if event != "snapshot" || updated["outputTruncated"] != true || !strings.HasSuffix(output, "-new-tail-1") {
		t.Fatalf("first updated preview = %q truncated=%v tail=%q", event, updated["outputTruncated"], output[max(0, len(output)-32):])
	}
	if len([]rune(output)) > subAgentDetailOutputLimit {
		t.Fatalf("preview exceeds bound: %d runes", len([]rune(output)))
	}

	teammate.AppendText("-new-tail-2")
	event, payload = nextSubAgentSSE(t, reader)
	updated, _ = payload["agent"].(map[string]any)
	output, _ = updated["output"].(string)
	if event != "snapshot" || !strings.HasSuffix(output, "-new-tail-2") {
		t.Fatalf("second updated preview = %q tail=%q", event, output[max(0, len(output)-32):])
	}
}

func nextSubAgentSSE(t *testing.T, reader *bufio.Reader) (string, map[string]any) {
	t.Helper()
	type received struct {
		event   string
		payload map[string]any
		err     error
	}
	result := make(chan received, 1)
	go func() {
		var event string
		var data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				result <- received{err: err}
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if event == "" || data == "" {
					continue
				}
				payload := map[string]any{}
				if err := json.Unmarshal([]byte(data), &payload); err != nil {
					result <- received{err: err}
					return
				}
				result <- received{event: event, payload: payload}
				return
			}
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case received := <-result:
		if received.err != nil {
			t.Fatalf("read sub-agent SSE: %v", received.err)
		}
		return received.event, received.payload
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for sub-agent SSE")
	}
	return "", nil
}

func TestDesktopSubAgentPanelLivesInHeaderAndLoadsOutput(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(index)
	headerStart := strings.Index(page, `id="viewTabs"`)
	chatStart := strings.Index(page, `id="chatArea"`)
	statusStart := strings.Index(page, `<div class="status-row">`)
	inputStart := strings.Index(page, `<div class="input-area">`)
	if headerStart < 0 || chatStart <= headerStart || !strings.Contains(page[headerStart:chatStart], `id="agentStatusAnchor"`) {
		t.Fatal("sub-agent status trigger is not mounted in the conversation header")
	}
	if statusStart < 0 || inputStart <= statusStart {
		t.Fatal("cannot locate legacy status row")
	}
	if strings.Contains(page[statusStart:inputStart], `id="statusChip"`) {
		t.Fatal("sub-agent status trigger is still mounted above the composer")
	}
	readerStart := strings.Index(page, `<div class="subagent-detail-overlay"`)
	if readerStart < 0 {
		t.Fatal("sub-agent conversation reader is missing")
	}
	readerEnd := strings.Index(page[readerStart:], "</section>")
	if readerEnd < 0 {
		t.Fatal("sub-agent conversation region is incomplete")
	}
	reader := page[readerStart : readerStart+readerEnd]
	for _, want := range []string{`id="subAgentDetailOverlay"`, `data-presentation="page"`, `role="region"`, `id="subAgentDetailBody"`, `tabindex="0"`, `aria-live="off"`, `id="subAgentBack"`, `id="subAgentDetailMetrics"`, `id="subAgentDetailInfo"`, `id="subAgentDetailFooterActions"`} {
		if !strings.Contains(reader, want) {
			t.Fatalf("sub-agent reader markup missing %q", want)
		}
	}
	if strings.Contains(reader, `aria-modal=`) || strings.Contains(reader, `role="dialog"`) {
		t.Fatal("read-only child page must not inherit modal semantics")
	}
	chatScript := strings.Index(page, `<script src="chat.js`)
	readerScript := strings.Index(page, `<script src="subagent_view.js`)
	if chatScript < 0 || readerScript <= chatScript {
		t.Fatal("sub-agent reader must load after its production Markdown formatter")
	}
	runSubagentConversationUI(t, "panel_lifecycle")
}
