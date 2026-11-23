package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestQueuedSteerAcknowledgementSurvivesWorkerCompletion(t *testing.T) {
	// Once handleSteer is blocked delivering the request, keep this goroutine
	// running long enough to buffer the ACK and close done before it resumes.
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	for attempt := range 64 {
		synctest.Test(t, func(t *testing.T) {
			worker := &isolatedActiveTurn{done: make(chan struct{}), steer: make(chan IsolatedSteerRequest)}
			s := &Server{loop: &agent.Loop{}, activeSessionID: "busy-session", workerTurns: map[string]*isolatedActiveTurn{"busy-session": worker}}
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/steer", bytes.NewBufferString(`{"sessionId":"busy-session","input":"accepted before completion"}`))
			go s.handleSteer(rr, r)
			synctest.Wait() // The real handler is durably blocked sending to steer.
			request := <-worker.steer
			request.Reply <- true
			close(worker.done)
			if len(request.Reply) != 1 {
				t.Fatal("fixture did not retain the ACK until worker completion")
			}
			synctest.Wait()
			if rr.Code != http.StatusAccepted || !bytes.Contains(rr.Body.Bytes(), []byte(`"accepted":true`)) {
				t.Fatalf("attempt %d: acknowledged steer became %d: %s", attempt, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestQueuedSteerWorkerCompletionWithoutAcceptanceRemainsConflict(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	for _, declined := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			worker := &isolatedActiveTurn{done: make(chan struct{}), steer: make(chan IsolatedSteerRequest)}
			s := &Server{loop: &agent.Loop{}, activeSessionID: "busy-session", workerTurns: map[string]*isolatedActiveTurn{"busy-session": worker}}
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/steer", bytes.NewBufferString(`{"sessionId":"busy-session","input":"not accepted"}`))
			go s.handleSteer(rr, r)
			synctest.Wait()
			request := <-worker.steer
			if declined {
				request.Reply <- false
			}
			close(worker.done)
			synctest.Wait()
			if rr.Code != http.StatusConflict {
				t.Fatalf("declined=%t: unaccepted steer became %d: %s", declined, rr.Code, rr.Body.String())
			}
		})
	}
}
