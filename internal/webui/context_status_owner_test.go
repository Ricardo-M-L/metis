package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestStatusContextOwnerTracksParentLoopAcrossIsolatedTurns(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workDirs := map[string]string{"session-a": t.TempDir(), "session-b": t.TempDir()}
	for id, workDir := range workDirs {
		if err := store.WriteHeaderFull(session.Header{
			ID: id, WorkDir: workDir, Provider: "wire", Model: "model",
			System: "system", Mode: string(permission.ModeAsk), Status: "idle",
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: id}}}); err != nil {
			t.Fatal(err)
		}
	}
	_, history, err := store.Load("session-a")
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.ContextWindow = 128_000
	loop.Restore(history)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 2), release: release}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "session-a", ProviderName: "wire", FreshPermissionMode: permission.ModeAsk,
		IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{Executable: "/ignored-in-test", MaxParallel: 2},
	})
	readStatus := func(path string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	activate := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/activate", strings.NewReader(`{"id":"`+id+`"}`)))
		return rr
	}
	startTurn := func(id string) <-chan *httptest.ResponseRecorder {
		response := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"sessionId":"`+id+`","input":"work"}`)))
			response <- rr
		}()
		select {
		case request := <-runner.started:
			if request.SessionID != id {
				t.Fatalf("worker started for %q, want %q", request.SessionID, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("worker for %q did not start", id)
		}
		return response
	}

	initial := readStatus("/api/status")
	if initial["contextSessionId"] != "session-a" || initial["contextUsed"].(float64) <= 0 {
		t.Fatalf("initial Loop context lacks its owner: %#v", initial)
	}
	// Selection metadata cannot assign the Loop's already-loaded context to B.
	s.stateMu.Lock()
	s.activeSessionID = "session-b"
	s.stateMu.Unlock()
	if selectedB := readStatus("/api/status"); selectedB["activeSessionId"] != "session-b" || selectedB["contextSessionId"] != "session-a" {
		t.Fatalf("selected session relabeled parent Loop context: %#v", selectedB)
	}
	s.stateMu.Lock()
	s.activeSessionID = "session-a"
	s.stateMu.Unlock()
	// Viewing or running B cannot re-label the parent Loop's A context.
	responseB := startTurn("session-b")
	viewingB := readStatus("/api/status?sessionId=session-b")
	if viewingB["contextSessionId"] != "session-a" || viewingB["activeSessionId"] != "session-a" {
		t.Fatalf("worker B borrowed parent Loop context: %#v", viewingB)
	}

	responseA := startTurn("session-a")
	if status := readStatus("/api/status"); status["contextSessionId"] != nil {
		t.Fatalf("stale parent context still attributed during worker A: %#v", status)
	}
	if rr := activate("session-a"); rr.Code != http.StatusOK {
		t.Fatalf("activate while worker A runs = %d: %s", rr.Code, rr.Body.String())
	}
	if status := readStatus("/api/status"); status["contextSessionId"] != nil {
		t.Fatalf("activation borrowed the running worker's context: %#v", status)
	}
	if err := store.AppendMessage("session-a", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("later worker context ", 120)}}}); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	for _, response := range []<-chan *httptest.ResponseRecorder{responseA, responseB} {
		select {
		case rr := <-response:
			if rr.Code != http.StatusOK {
				t.Fatalf("worker completed with %d: %s", rr.Code, rr.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not finish")
		}
	}
	if status := readStatus("/api/status"); status["contextSessionId"] != nil {
		t.Fatalf("stale parent context was reattributed after worker exit: %#v", status)
	}
	if rr := activate("session-a"); rr.Code != http.StatusOK {
		t.Fatalf("reactivate A = %d: %s", rr.Code, rr.Body.String())
	}
	refreshed := readStatus("/api/status")
	if refreshed["contextSessionId"] != "session-a" || refreshed["contextUsed"].(float64) <= initial["contextUsed"].(float64) {
		t.Fatalf("activation did not refresh A context: %#v", refreshed)
	}
	if rr := activate("session-b"); rr.Code != http.StatusOK {
		t.Fatalf("activate B = %d: %s", rr.Code, rr.Body.String())
	}
	if status := readStatus("/api/status"); status["contextSessionId"] != "session-b" || status["activeSessionId"] != "session-b" {
		t.Fatalf("context owner did not follow Loop activation: %#v", status)
	}
}

func TestStatusOmitsContextOwnerWithoutSessionBinding(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.AppendUser("unattributed history")
	s := NewServer("127.0.0.1:0", loop, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, claimed := payload["contextSessionId"]; claimed {
		t.Fatalf("unbound Loop claimed a session: %#v", payload)
	}
}

func TestReadContextStatusRejectsOwnershipChangesDuringLoopRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Server)
	}{
		{"owner changed", func(s *Server) { s.setContextOwnerAfterActivationLocked("session-b", 0) }},
		{"owner changed and restored", func(s *Server) {
			s.setContextOwnerAfterActivationLocked("session-b", 0)
			s.setContextOwnerAfterActivationLocked("session-a", 0)
		}},
		{"same owner reloaded", func(s *Server) { s.setContextOwnerAfterActivationLocked("session-a", 0) }},
		{"worker epoch changed", func(s *Server) { s.contextWorkerEpochs["session-a"]++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				loop:             agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2),
				contextSessionID: "session-a", contextWorkerEpochs: make(map[string]uint64),
				workerTurns: make(map[string]*isolatedActiveTurn),
			}
			reading := make(chan struct{})
			release := make(chan struct{})
			done := make(chan contextStatusSnapshot, 1)
			go func() {
				done <- s.readContextStatus(func() contextStatusSnapshot {
					close(reading)
					<-release
					return contextStatusSnapshot{used: 123, window: 128_000, compactThreshold: 0.8, compactAtTokens: 102_400}
				})
			}()
			<-reading
			s.contextMu.Lock()
			tc.change(s)
			s.contextMu.Unlock()
			close(release)
			select {
			case got := <-done:
				if got != (contextStatusSnapshot{}) {
					t.Fatalf("stale Loop context leaked after %s: %+v", tc.name, got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("context read did not finish")
			}
		})
	}
}

func TestReadContextStatusDoesNotBlockIndependentWorkerRegistration(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"session-a", "session-b"} {
		if err := store.WriteHeaderFull(session.Header{
			ID: id, WorkDir: t.TempDir(), Provider: "wire", Model: "model",
			System: "system", Mode: string(permission.ModeAsk), Status: "idle",
		}); err != nil {
			t.Fatal(err)
		}
	}
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	runnerRelease := make(chan struct{})
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 1), release: runnerRelease}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "session-a", ProviderName: "wire", FreshPermissionMode: permission.ModeAsk,
		IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{Executable: "/ignored-in-test", MaxParallel: 2},
	})
	reading := make(chan struct{})
	releaseRead := make(chan struct{})
	statusDone := make(chan contextStatusSnapshot, 1)
	go func() {
		statusDone <- s.readContextStatus(func() contextStatusSnapshot {
			close(reading)
			<-releaseRead
			return contextStatusSnapshot{used: 123, window: 128_000}
		})
	}()
	<-reading
	turnDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"sessionId":"session-b","input":"work"}`)))
		turnDone <- rr
	}()
	select {
	case request := <-runner.started:
		if request.SessionID != "session-b" {
			t.Fatalf("worker started for %q", request.SessionID)
		}
	case <-time.After(2 * time.Second):
		close(releaseRead)
		close(runnerRelease)
		t.Fatal("independent worker registration waited for the context read")
	}
	close(releaseRead)
	if status := <-statusDone; status.sessionID != "session-a" || status.used != 123 {
		t.Fatalf("unrelated worker invalidated parent context: %+v", status)
	}
	close(runnerRelease)
	select {
	case rr := <-turnDone:
		if rr.Code != http.StatusOK {
			t.Fatalf("worker turn = %d: %s", rr.Code, rr.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish")
	}
}
