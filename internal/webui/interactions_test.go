package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type interactionSnapshotResponse struct {
	Permissions []permissionSnapshot `json:"permissions"`
	Asks        []askSnapshot        `json:"asks"`
}

func readInteractionSnapshot(t *testing.T, s *Server, sessionID string) interactionSnapshotResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/interactions?sessionId="+sessionID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d: %s", rr.Code, rr.Body.String())
	}
	var snapshot interactionSnapshotResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func publishTestInteractions(t *testing.T, s *Server, ctx context.Context, owner string) (chan agent.PermissionDecision, chan string) {
	t.Helper()
	permissionReply := make(chan agent.PermissionDecision, 1)
	askReply := make(chan string, 1)
	if !s.publishInteraction(ctx, owner, agent.Event{
		Kind: agent.EventPermissionRequest, PermissionTool: "Write", PermissionReason: "confirm " + owner,
		PermissionInput: map[string]any{"path": owner + ".txt", "api_key": "never-show-this"},
		PermissionReply: permissionReply,
	}) {
		t.Fatal("permission event was not handled")
	}
	if !s.publishInteraction(ctx, owner, agent.Event{
		Kind: agent.EventAskUser, AskUserQuestion: "choose " + owner,
		AskUserOptions: []string{"one", "two"}, AskUserAllowFreeform: true, AskUserReply: askReply,
	}) {
		t.Fatal("ask event was not handled")
	}
	return permissionReply, askReply
}

func TestInteractionsSnapshotRestoresOnlyOwnedPresentation(t *testing.T) {
	s, _ := testServer(t)
	t.Cleanup(s.cancelPendingInteractions)
	publishTestInteractions(t, s, context.Background(), "session-a")
	publishTestInteractions(t, s, context.Background(), "session-b")
	snapshot := readInteractionSnapshot(t, s, "session-a")
	if len(snapshot.Permissions) != 1 || len(snapshot.Asks) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	permission := snapshot.Permissions[0]
	if permission.Session != "session-a" || permission.Tool != "Write" || permission.Reason != "confirm session-a" || !strings.Contains(permission.Input, "session-a.txt") {
		t.Fatalf("permission presentation = %+v", permission)
	}
	if strings.Contains(permission.Input, "never-show-this") {
		t.Fatalf("snapshot exposed an authorization secret: %s", permission.Input)
	}
	ask := snapshot.Asks[0]
	if ask.Session != "session-a" || ask.Question != "choose session-a" || len(ask.Options) != 2 || !ask.AllowFreeform {
		t.Fatalf("ask presentation = %+v", ask)
	}
	s.permMu.Lock()
	pending := s.pendingPerms[permission.ID]
	if pending.event.PermissionReply != nil || pending.event.AskUserReply != nil {
		t.Error("snapshot queue retained reply channels in presentation event")
	}
	s.permMu.Unlock()
	empty := readInteractionSnapshot(t, s, "session-other")
	if empty.Permissions == nil || empty.Asks == nil || len(empty.Permissions)+len(empty.Asks) != 0 {
		t.Fatalf("empty snapshot should contain two empty arrays: %+v", empty)
	}
}

func TestInteractionsWrongSessionCannotConsumeAnotherCard(t *testing.T) {
	s, _ := testServer(t)
	t.Cleanup(s.cancelPendingInteractions)
	permissionReply, askReply := publishTestInteractions(t, s, context.Background(), "session-a")
	snapshot := readInteractionSnapshot(t, s, "session-a")
	cases := []struct {
		path string
		id   string
		body string
	}{
		{"/api/permission", snapshot.Permissions[0].ID, `"approve":true`},
		{"/api/ask", snapshot.Asks[0].ID, `"answer":"one"`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			for _, attempt := range []struct {
				owner string
				want  int
			}{{"session-b", http.StatusConflict}, {"session-a", http.StatusOK}, {"session-a", http.StatusNotFound}} {
				rr := httptest.NewRecorder()
				body := fmt.Sprintf(`{"id":%q,"sessionId":%q,%s}`, tc.id, attempt.owner, tc.body)
				s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body)))
				if rr.Code != attempt.want {
					t.Fatalf("owner %s status = %d, want %d: %s", attempt.owner, rr.Code, attempt.want, rr.Body.String())
				}
			}
		})
	}
	if got := <-permissionReply; got != agent.PermissionDecisionAllow {
		t.Fatalf("permission = %v", got)
	}
	if got := <-askReply; got != "one" {
		t.Fatalf("answer = %q", got)
	}
}

func TestCancelSessionInteractionsLeavesOtherWorkersWaiting(t *testing.T) {
	s, _ := testServer(t)
	t.Cleanup(s.cancelPendingInteractions)
	aPermission, aAsk := publishTestInteractions(t, s, context.Background(), "session-a")
	bPermission, bAsk := publishTestInteractions(t, s, context.Background(), "session-b")
	snapshot := readInteractionSnapshot(t, s, "session-a")
	s.permMu.Lock()
	permissionDone := s.pendingPerms[snapshot.Permissions[0].ID].done
	s.permMu.Unlock()
	s.askMu.Lock()
	askDone := s.pendingAsks[snapshot.Asks[0].ID].done
	s.askMu.Unlock()
	events := s.hub.subscribe()
	defer s.hub.unsubscribe(events)
	s.cancelSessionInteractions("session-a")
	if got := <-aPermission; got != agent.PermissionDecisionDeny {
		t.Fatalf("cancel permission = %v", got)
	}
	if got := <-aAsk; got != "" {
		t.Fatalf("cancel answer = %q", got)
	}
	for _, done := range []chan struct{}{permissionDone, askDone} {
		select {
		case <-done:
		default:
			t.Fatal("cancel did not release the interaction timer")
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case event := <-events:
			if event.session != "session-a" || event.extra["kind"] != "interaction_resolved" {
				t.Fatalf("cancel event = %+v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("missing interaction resolution event")
		}
	}
	if got := readInteractionSnapshot(t, s, "session-a"); len(got.Permissions)+len(got.Asks) != 0 {
		t.Fatalf("cancelled cards remain: %+v", got)
	}
	if got := readInteractionSnapshot(t, s, "session-b"); len(got.Permissions) != 1 || len(got.Asks) != 1 {
		t.Fatalf("other session lost cards: %+v", got)
	}
	select {
	case got := <-bPermission:
		t.Fatalf("other session permission unexpectedly resolved: %v", got)
	default:
	}
	select {
	case got := <-bAsk:
		t.Fatalf("other session question unexpectedly resolved: %q", got)
	default:
	}
}

func TestInteractionContextCancellationExpiresWithoutManualStop(t *testing.T) {
	s, _ := testServer(t)
	t.Cleanup(s.cancelPendingInteractions)
	ctx, cancel := context.WithCancel(context.Background())
	permissionReply, askReply := publishTestInteractions(t, s, ctx, "session-a")
	cancel()
	select {
	case got := <-permissionReply:
		if got != agent.PermissionDecisionDeny {
			t.Fatalf("permission = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation left permission pending")
	}
	select {
	case got := <-askReply:
		if got != "" {
			t.Fatalf("answer = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation left question pending")
	}
	if snapshot := readInteractionSnapshot(t, s, "session-a"); len(snapshot.Permissions)+len(snapshot.Asks) != 0 {
		t.Fatalf("expired snapshot = %+v", snapshot)
	}
}

func TestConcurrentInteractionResolutionConsumesExactlyOnce(t *testing.T) {
	s, _ := testServer(t)
	t.Cleanup(s.cancelPendingInteractions)
	permissionReply, _ := publishTestInteractions(t, s, context.Background(), "session-a")
	id := readInteractionSnapshot(t, s, "session-a").Permissions[0].ID
	const requests = 16
	results := make(chan int, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			body := fmt.Sprintf(`{"id":%q,"sessionId":"session-a","approve":true}`, id)
			s.handlePermission(rr, httptest.NewRequest(http.MethodPost, "/api/permission", strings.NewReader(body)))
			results <- rr.Code
		}()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for status := range results {
		if status == http.StatusOK {
			succeeded++
		} else if status != http.StatusNotFound {
			t.Fatalf("unexpected resolution status: %d", status)
		}
	}
	if succeeded != 1 || len(permissionReply) != 1 {
		t.Fatalf("successful requests = %d, replies = %d", succeeded, len(permissionReply))
	}
}

func TestInteractionQueueIsBoundedAndCanceledRequestsAreDenied(t *testing.T) {
	s, _ := testServer(t)
	t.Cleanup(s.cancelPendingInteractions)
	for i := 0; i < pendingInteractionLimit; i++ {
		publishTestInteractions(t, s, context.Background(), "session-a")
	}
	permissionReply, askReply := publishTestInteractions(t, s, context.Background(), "overflow")
	if got := <-permissionReply; got != agent.PermissionDecisionDeny {
		t.Fatalf("overflow permission = %v", got)
	}
	if got := <-askReply; got != "" {
		t.Fatalf("overflow question = %q", got)
	}
	if got := readInteractionSnapshot(t, s, "overflow"); len(got.Permissions)+len(got.Asks) != 0 {
		t.Fatalf("overflow registered cards: %+v", got)
	}
	s.cancelPendingInteractions()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	permissionReply, askReply = publishTestInteractions(t, s, ctx, "already-cancelled")
	if got := <-permissionReply; got != agent.PermissionDecisionDeny {
		t.Fatalf("cancelled permission = %v", got)
	}
	if got := <-askReply; got != "" {
		t.Fatalf("cancelled question = %q", got)
	}
	if got := readInteractionSnapshot(t, s, "already-cancelled"); len(got.Permissions)+len(got.Asks) != 0 {
		t.Fatalf("cancelled context registered cards: %+v", got)
	}
}

type interactiveIsolatedDecision struct {
	session  string
	decision agent.PermissionDecision
}

type interactiveIsolatedRunner struct {
	started   chan string
	decisions chan interactiveIsolatedDecision
	questions chan string
}

func (*interactiveIsolatedRunner) Eligible(header *session.Header) bool {
	return (&processIsolatedTurnRunner{}).Eligible(header)
}

func (runner *interactiveIsolatedRunner) Run(ctx context.Context, request IsolatedTurnRequest) (IsolatedTurnResult, error) {
	permissionReply := make(chan agent.PermissionDecision, 1)
	request.OnEvent(agent.Event{
		Kind: agent.EventPermissionRequest, PermissionTool: "Write", ToolName: "Write",
		PermissionInput: map[string]any{"path": request.WorkDir + "/answer.txt"},
		PermissionReply: permissionReply,
	})
	runner.started <- request.SessionID
	select {
	case decision := <-permissionReply:
		runner.decisions <- interactiveIsolatedDecision{request.SessionID, decision}
	case <-ctx.Done():
		return IsolatedTurnResult{Stopped: true}, ctx.Err()
	}
	answerReply := make(chan string, 1)
	request.OnEvent(agent.Event{
		Kind: agent.EventAskUser, AskUserQuestion: "next action for " + request.SessionID,
		AskUserAllowFreeform: true, AskUserReply: answerReply,
	})
	runner.questions <- request.SessionID
	select {
	case answer := <-answerReply:
		if ctx.Err() != nil {
			return IsolatedTurnResult{Stopped: true}, ctx.Err()
		}
		return IsolatedTurnResult{Text: "answered: " + answer}, nil
	case <-ctx.Done():
		return IsolatedTurnResult{Stopped: true}, ctx.Err()
	}
}

func TestDefaultWorkspaceTurnsKeepApprovalsAndQuestionsIndependent(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"interactive-a", "interactive-b"} {
		if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: t.TempDir(), Mode: string(permission.ModeDefault), Status: "idle"}); err != nil {
			t.Fatal(err)
		}
	}
	runner := &interactiveIsolatedRunner{
		started: make(chan string, 2), decisions: make(chan interactiveIsolatedDecision, 2), questions: make(chan string, 2),
	}
	server := NewServer("127.0.0.1:0", agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2), store, RuntimeBindings{
		IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{Executable: "/ignored-in-test", MaxParallel: 2},
	})
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		server.cancelPendingInteractions()
		group.Wait()
	})
	type responseResult struct {
		session  string
		response *httptest.ResponseRecorder
	}
	responses := make(chan responseResult, 2)
	for _, id := range []string{"interactive-a", "interactive-b"} {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			response := httptest.NewRecorder()
			body := fmt.Sprintf(`{"sessionId":%q,"input":"work requiring approval"}`, id)
			request := httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(body)).WithContext(ctx)
			server.handler().ServeHTTP(response, request)
			responses <- responseResult{id, response}
		}(id)
	}
	started := make(map[string]bool)
	for i := 0; i < 2; i++ {
		select {
		case id := <-runner.started:
			started[id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("default-mode workspaces did not reach approval concurrently; started = %v", started)
		}
	}
	if !started["interactive-a"] || !started["interactive-b"] {
		t.Fatalf("started workers = %v", started)
	}
	for _, item := range []struct {
		id      string
		approve bool
	}{{"interactive-a", true}, {"interactive-b", false}} {
		snapshot := readInteractionSnapshot(t, server, item.id)
		if len(snapshot.Permissions) != 1 {
			t.Fatalf("%s pending approvals = %+v", item.id, snapshot)
		}
		rr := httptest.NewRecorder()
		body := fmt.Sprintf(`{"id":%q,"sessionId":%q,"approve":%t}`, snapshot.Permissions[0].ID, item.id, item.approve)
		server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/permission", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("resolve %s: %d %s", item.id, rr.Code, rr.Body.String())
		}
	}
	decisions := make(map[string]agent.PermissionDecision)
	for i := 0; i < 2; i++ {
		select {
		case result := <-runner.decisions:
			decisions[result.session] = result.decision
		case <-time.After(time.Second):
			t.Fatal("worker did not receive its permission decision")
		}
		select {
		case <-runner.questions:
		case <-time.After(time.Second):
			t.Fatal("worker did not reach its follow-up question")
		}
	}
	if decisions["interactive-a"] != agent.PermissionDecisionAllow || decisions["interactive-b"] != agent.PermissionDecisionDeny {
		t.Fatalf("cross-session approval decisions: %v", decisions)
	}
	stop := httptest.NewRecorder()
	server.handler().ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "/api/stop", strings.NewReader(`{"sessionId":"interactive-a"}`)))
	if stop.Code != http.StatusOK {
		t.Fatalf("stop A: %d %s", stop.Code, stop.Body.String())
	}
	if snapshot := readInteractionSnapshot(t, server, "interactive-a"); len(snapshot.Permissions)+len(snapshot.Asks) != 0 {
		t.Fatalf("stopped worker retained cards: %+v", snapshot)
	}
	bSnapshot := readInteractionSnapshot(t, server, "interactive-b")
	if len(bSnapshot.Asks) != 1 || bSnapshot.Asks[0].Question != "next action for interactive-b" {
		t.Fatalf("stopping A lost B's question: %+v", bSnapshot)
	}
	answer := httptest.NewRecorder()
	body := fmt.Sprintf(`{"id":%q,"sessionId":"interactive-b","answer":"retained answer"}`, bSnapshot.Asks[0].ID)
	server.handler().ServeHTTP(answer, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body)))
	if answer.Code != http.StatusOK {
		t.Fatalf("answer B: %d %s", answer.Code, answer.Body.String())
	}
	for i := 0; i < 2; i++ {
		select {
		case result := <-responses:
			if result.response.Code != http.StatusOK {
				t.Fatalf("turn %s: %d %s", result.session, result.response.Code, result.response.Body.String())
			}
			var body struct {
				Stopped bool   `json:"stopped"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal(result.response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if result.session == "interactive-a" && !body.Stopped {
				t.Fatal("cancelled A was not reported stopped")
			}
			if result.session == "interactive-b" && (body.Stopped || body.Text != "answered: retained answer") {
				t.Fatalf("B did not finish independently: %+v", body)
			}
		case <-time.After(time.Second):
			t.Fatal("interactive turn did not return")
		}
	}
}
