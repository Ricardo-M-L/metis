package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDesktopIncompleteTurnRetainsTypedReasonAndFailureStatus(t *testing.T) {
	for _, reason := range []string{"max_iterations", "budget_usd", "turn_wall_clock", "loop_detected", "max_tokens"} {
		t.Run(reason, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			provider := &incompleteTurnProvider{
				activationTestProvider: activationTestProvider{name: "iteration-fixture", model: "iteration-fixture"},
				stopReason:             reason,
			}
			loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
			loop.Model = provider.ModelID()
			const id = "incomplete"
			if err := store.WriteHeaderFull(session.Header{ID: id, Provider: provider.Name(), Model: loop.Model, System: "system", Status: "idle"}); err != nil {
				t.Fatal(err)
			}
			server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: provider.Name(), InitialSessionID: id})
			response := httptest.NewRecorder()
			server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"sessionId":"incomplete","input":"work"}`)))
			assertDesktopIncompleteFailure(t, response, store, id, reason)
			server.hub.mu.RLock()
			defer server.hub.mu.RUnlock()
			for _, event := range server.hub.replay {
				if event.ev.Kind == agent.EventError {
					t.Fatalf("incomplete terminal emitted an API error: %+v", event.ev)
				}
			}
		})
	}
}

type terminalIsolatedRunner struct {
	result IsolatedTurnResult
	err    error
}

func (*terminalIsolatedRunner) Eligible(*session.Header) bool { return true }
func (r *terminalIsolatedRunner) Run(context.Context, IsolatedTurnRequest) (IsolatedTurnResult, error) {
	return r.result, r.err
}

func TestDesktopIsolatedIncompleteTurnNeverPersistsCompleted(t *testing.T) {
	for _, workerErr := range []error{nil, errors.Join(&agent.IncompleteRunError{Reason: "max_iterations"}, errors.New("worker exit status 11"))} {
		name := "successful worker exit"
		if workerErr != nil {
			name = "nonzero worker exit"
		}
		t.Run(name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "isolated-incomplete"
			if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: t.TempDir(), Mode: string(permission.ModeFullAccess), Status: "idle"}); err != nil {
				t.Fatal(err)
			}
			server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{IsolatedRunner: &terminalIsolatedRunner{
				result: IsolatedTurnResult{Text: "partial work", Done: &agent.Event{Kind: agent.EventLoopDone, StopReason: "max_iterations"}, Checkpointed: true},
				err:    workerErr,
			}})
			response := httptest.NewRecorder()
			server.handleIsolatedTurn(response, httptest.NewRequest(http.MethodPost, "/api/turns", nil), id, "work", t.TempDir())
			assertDesktopIncompleteFailure(t, response, store, id, "max_iterations")
			if workerErr != nil {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(body.Error, workerErr.Error()) {
					t.Fatalf("worker failure detail lost: %s", response.Body.String())
				}
			}
			server.hub.mu.RLock()
			defer server.hub.mu.RUnlock()
			if len(server.hub.replay) != 1 || server.hub.replay[0].ev.Kind != agent.EventLoopDone || server.hub.replay[0].ev.StopReason != "max_iterations" {
				t.Fatalf("incomplete worker lost typed terminal: %+v", server.hub.replay)
			}
		})
	}
}

func TestDesktopIsolatedCompletedEventCannotMaskWorkerFailure(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "isolated-failed"
	if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: t.TempDir(), Mode: string(permission.ModeFullAccess), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{IsolatedRunner: &terminalIsolatedRunner{
		result: IsolatedTurnResult{Done: &agent.Event{Kind: agent.EventLoopDone, StopReason: "end_turn"}},
		err:    errors.New("worker exit status 47"),
	}})
	response := httptest.NewRecorder()
	server.handleIsolatedTurn(response, httptest.NewRequest(http.MethodPost, "/api/turns", nil), id, "work", t.TempDir())
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "worker exit status 47") || strings.Contains(response.Body.String(), `"incomplete":true`) {
		t.Fatalf("worker failure treated as completion: status=%d body=%s", response.Code, response.Body.String())
	}
	header, _, err := store.LoadHeader(id)
	if err != nil || header.Status != "failed" {
		t.Fatalf("worker failure status=%+v err=%v", header, err)
	}
}

func TestDesktopIncompleteFrameCannotMaskUnclassifiedWorkerFailure(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "incomplete-frame-fatal-exit"
	if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: t.TempDir(), Mode: string(permission.ModeFullAccess), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{IsolatedRunner: &terminalIsolatedRunner{
		result: IsolatedTurnResult{Done: &agent.Event{Kind: agent.EventLoopDone, StopReason: "max_iterations"}},
		err:    errors.New("worker persistence failed: exit status 47"),
	}})
	response := httptest.NewRecorder()
	server.handleIsolatedTurn(response, httptest.NewRequest(http.MethodPost, "/api/turns", nil), id, "work", t.TempDir())
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), `"incomplete":true`) || !strings.Contains(response.Body.String(), "persistence failed") {
		t.Fatalf("fatal failure downgraded to a local limit: status=%d body=%s", response.Code, response.Body.String())
	}
	server.hub.mu.RLock()
	defer server.hub.mu.RUnlock()
	if len(server.hub.replay) != 1 || server.hub.replay[0].ev.Kind != agent.EventError {
		t.Fatalf("fatal worker error hidden by incomplete frame: %+v", server.hub.replay)
	}
}

func assertDesktopIncompleteFailure(t *testing.T, response *httptest.ResponseRecorder, store *session.Store, id, reason string) {
	t.Helper()
	var body struct {
		Error      string `json:"error"`
		Incomplete bool   `json:"incomplete"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusBadGateway || body.Error == "" || !body.Incomplete || body.StopReason != reason {
		t.Fatalf("incomplete status=%d body=%s", response.Code, response.Body.String())
	}
	header, _, err := store.LoadHeader(id)
	if err != nil || header.Status != "failed" {
		t.Fatalf("incomplete durable status=%+v err=%v", header, err)
	}
}
