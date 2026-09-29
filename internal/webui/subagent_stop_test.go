package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestQueuedSubAgentCountsAsActive(t *testing.T) {
	s, _, _, _ := queuedSubAgentServer(t, agent.StatusQueued)
	s.activeSessionID = "session-a"
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var result struct {
		SubAgents int `json:"subAgents"`
		Agents    []struct {
			Status string `json:"status"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || result.SubAgents != 1 || len(result.Agents) != 1 || result.Agents[0].Status != "queued" {
		t.Fatalf("queued status=%d %+v", rr.Code, result)
	}
}

func TestWorkerSubAgentCreationPrecedesPeriodicSnapshot(t *testing.T) {
	s, _ := testServer(t)
	s.recordWorkerSubAgentLifecycle("session-a", agent.Event{Kind: agent.EventSubAgentStart, SubAgentID: "agt-new", SubAgentName: "new", SubAgentBackground: true, SubAgentStatus: "queued"})
	assertStatus := func(want string) {
		t.Helper()
		view, ok := s.subAgentDetailView("session-a", "agt-new")
		if !ok || view.Status != want || !view.Background {
			t.Fatalf("detail=%+v found=%v want=%s", view, ok, want)
		}
		if _, ok := s.subAgentDetailView("session-b", "agt-new"); ok {
			t.Fatal("new identity leaked to another session")
		}
	}
	assertStatus("queued")
	// A status captured before registration must not erase the start event.
	s.setWorkerSnapshot("session-a", desktopipc.Status{})
	assertStatus("queued")
	s.recordWorkerSubAgentLifecycle("session-a", agent.Event{Kind: agent.EventSubAgentStart, SubAgentID: "agt-new", SubAgentBackground: true, SubAgentStatus: "running"})
	s.setWorkerSnapshot("session-a", desktopipc.Status{SubAgents: 1, Agents: []desktopipc.Subagent{{AgentID: "agt-new", Background: true, Status: "queued"}}})
	assertStatus("running")
	s.setWorkerSnapshot("session-a", desktopipc.Status{SubAgents: 1, Agents: []desktopipc.Subagent{{AgentID: "agt-new", Background: true, Status: "running"}}})
	assertStatus("running")
	if len(s.workerSnapshots["session-a"].unobserved) != 0 {
		t.Fatal("observed lifecycle identity was retained")
	}
	s.setWorkerSnapshot("session-a", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-new", Background: true, Status: "killed"}}})
	assertStatus("killed")
}

func queuedSubAgentServer(t *testing.T, status agent.TeammateStatus) (*Server, *agent.Teammate, *agent.Roster, context.Context) {
	t.Helper()
	_, store := testServer(t)
	roster := agent.NewRoster(2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	teammate := &agent.Teammate{Name: "queued", AgentID: "agt-queued", Started: time.Now(), Background: true}
	teammate.SetCancel(cancel)
	var err error
	if status == agent.StatusQueued {
		err = roster.RegisterQueued(teammate)
	} else {
		err = roster.Register(teammate)
	}
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := agent.NewSubAgentTranscript(store.Dir, teammate.AgentID,
		agent.NewSubAgentHeader(teammate.AgentID, "fixture", "session-a", teammate.Name, t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	return NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster}), teammate, roster, ctx
}

func TestSubAgentStopRoutesToOwningWorkerAndWaitsForAcknowledgement(t *testing.T) {
	s, _ := testServer(t)
	first := &isolatedActiveTurn{done: make(chan struct{}), steer: make(chan IsolatedSteerRequest, 1), cancel: func() { t.Error("canceled parent worker") }}
	second := &isolatedActiveTurn{done: make(chan struct{}), steer: make(chan IsolatedSteerRequest, 1), cancel: func() { t.Error("canceled foreign worker") }}
	s.workerTurns["session-a"], s.workerTurns["session-b"] = first, second
	s.setWorkerSnapshot("session-a", desktopipc.Status{SubAgents: 1, Agents: []desktopipc.Subagent{{AgentID: "agt-queued", Status: "queued"}}})
	s.setWorkerSnapshot("session-b", desktopipc.Status{SubAgents: 1, Agents: []desktopipc.Subagent{{AgentID: "agt-other", Status: "running"}}})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/subagents/agt-queued/stop?sessionId=session-b", nil))
	if rr.Code != http.StatusNotFound || len(first.steer) != 0 || len(second.steer) != 0 {
		t.Fatalf("foreign stop=%d requests=%d,%d", rr.Code, len(first.steer), len(second.steer))
	}
	for _, accepted := range []bool{false, true} {
		rr = httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/subagents/agt-queued/stop?sessionId=session-a", nil))
		}()
		select {
		case request := <-first.steer:
			if request.StopSubAgentID != "agt-queued" || request.Input != "" {
				t.Fatalf("control=%+v", request)
			}
			select {
			case <-done:
				t.Fatal("HTTP succeeded before worker acknowledgement")
			default:
			}
			if accepted {
				s.setWorkerSnapshot("session-a", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "agt-queued", Status: "killed"}}})
			}
			request.Reply <- accepted
		case <-time.After(time.Second):
			t.Fatal("no worker control request")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("HTTP did not finish after acknowledgement")
		}
		want := http.StatusConflict
		if accepted {
			want = http.StatusOK
		}
		if rr.Code != want {
			t.Fatalf("ack=%v status=%d body=%s", accepted, rr.Code, rr.Body.String())
		}
	}
	if len(second.steer) != 0 {
		t.Fatal("foreign worker received cancellation")
	}
}

func TestSubAgentStopRequiresOwnerAndCancelsOnlyChild(t *testing.T) {
	for _, status := range []agent.TeammateStatus{agent.StatusQueued, agent.StatusRunning} {
		t.Run(status.String(), func(t *testing.T) {
			s, teammate, _, child := queuedSubAgentServer(t, status)
			for _, tc := range []struct {
				method, session string
				want            int
			}{
				{http.MethodGet, "session-a", http.StatusMethodNotAllowed},
				{http.MethodPost, "session-b", http.StatusNotFound},
				{http.MethodPost, "", http.StatusBadRequest},
			} {
				rr := httptest.NewRecorder()
				s.handler().ServeHTTP(rr, httptest.NewRequest(tc.method, "/api/subagents/agt-queued/stop?sessionId="+tc.session, nil))
				if rr.Code != tc.want {
					t.Fatalf("%s session=%q = %d: %s", tc.method, tc.session, rr.Code, rr.Body.String())
				}
				if child.Err() != nil {
					t.Fatal("rejected request canceled the child")
				}
			}
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/subagents/agt-queued/stop?sessionId=session-a", nil))
			if rr.Code != http.StatusOK || child.Err() != context.Canceled {
				t.Fatalf("stop=%d child=%v body=%s", rr.Code, child.Err(), rr.Body.String())
			}
			teammate.Finish(agent.StatusKilled, "", child.Err(), "user kill")
			if view, ok := s.subAgentDetailView("session-a", teammate.AgentID); !ok || view.Status != "killed" {
				t.Fatalf("stopped detail=%+v found=%v", view, ok)
			}
		})
	}
}

func TestSubAgentDetailStreamKeepsQueuedAliveAndEmitsRunningWithoutOutput(t *testing.T) {
	s, teammate, roster, _ := queuedSubAgentServer(t, agent.StatusQueued)
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/subagents/agt-queued/events?sessionId=session-a", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	event, payload := nextSubAgentSSE(t, reader)
	if event != "snapshot" || payload["agent"].(map[string]any)["status"] != "queued" {
		t.Fatalf("initial=%s %#v", event, payload)
	}
	if err := roster.TryStartQueued(teammate, func() {}); err != nil {
		t.Fatal(err)
	}
	event, payload = nextSubAgentSSE(t, reader)
	if event != "snapshot" || payload["agent"].(map[string]any)["status"] != "running" {
		t.Fatalf("running=%s %#v", event, payload)
	}
	teammate.Finish(agent.StatusKilled, "", context.Canceled, "user kill")
	event, payload = nextSubAgentSSE(t, reader)
	if event != "terminal" || payload["agent"].(map[string]any)["status"] != "killed" {
		t.Fatalf("terminal=%s %#v", event, payload)
	}
}
