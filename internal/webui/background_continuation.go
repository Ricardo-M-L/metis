package webui

import (
	"context"
	"log"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// stopBackgroundContinuationLocked invalidates both a waiting wakeup and a
// continuation that has reserved the turn slot. Call with cancelMu held.
func (s *Server) stopBackgroundContinuationLocked() {
	s.backgroundGeneration++
	if s.backgroundCancel != nil {
		s.backgroundCancel()
	}
	s.backgroundCancel = nil
	s.backgroundSession = ""
}

// armBackgroundContinuation is called only after a successful explicit turn,
// with runMu held. The request context cannot own this watcher: net/http
// cancels it immediately after the ordinary response has been sent.
func (s *Server) armBackgroundContinuation(turnCtx context.Context, sessionID string) {
	if s.loop.Jobs == nil || s.loop.JobNotify == nil {
		return
	}
	s.cancelMu.Lock()
	if s.closing || turnCtx.Err() != nil {
		s.cancelMu.Unlock()
		return
	}
	s.stopBackgroundContinuationLocked()
	ctx, cancel := context.WithCancel(context.Background())
	s.backgroundCancel = cancel
	s.backgroundSession = sessionID
	generation := s.backgroundGeneration
	s.cancelMu.Unlock()
	go s.watchBackgroundContinuation(ctx, sessionID, generation, s.loop.Jobs)
}

func (s *Server) watchBackgroundContinuation(ctx context.Context, sessionID string, generation uint64, registry *jobs.Registry) {
	defer func() {
		s.cancelMu.Lock()
		if s.backgroundGeneration == generation {
			s.stopBackgroundContinuationLocked()
		}
		s.cancelMu.Unlock()
	}()
	for ctx.Err() == nil {
		if !registry.HasPendingNotifications() {
			// Remain armed for this successful session lifetime. A job may
			// already look terminal before its notification is published;
			// an empty queue and no running jobs are not proof of quiescence.
			select {
			case <-ctx.Done():
				return
			case <-registry.Wakeup():
			}
			continue
		}
		// The running Loop alone drains Notifications. The separate wakeup
		// signal never steals a completion from it. Waiting for runMu also
		// closes the race where completion arrives during Run's final unwind.
		s.runMu.Lock()
		s.stateMu.RLock()
		activeSession, workDir := s.activeSessionID, s.activeWorkDir
		s.stateMu.RUnlock()
		s.cancelMu.Lock()
		if ctx.Err() != nil || s.closing || s.backgroundGeneration != generation || activeSession != sessionID {
			s.cancelMu.Unlock()
			s.runMu.Unlock()
			return
		}
		if !registry.HasPendingNotifications() {
			s.cancelMu.Unlock()
			s.runMu.Unlock()
			continue
		}
		turnCtx, cancel := context.WithCancel(agent.WithCwd(ctx, workDir))
		turnDone := make(chan struct{})
		s.cancelTurn, s.runningSession, s.turnDone = cancel, sessionID, turnDone
		s.cancelMu.Unlock()

		metric := session.MessageMetric{
			Turn: s.nextMessageMetricTurn(sessionID, s.loop.History()), StartedAt: time.Now(),
		}
		alignTraceTurnFloor(sessionID, metric.Turn)
		s.hub.publish(sessionID, agent.Event{Kind: agent.EventInfo}, map[string]any{"backgroundContinuation": "started"})
		_, runErr, _, mayContinue := s.runAndPersistTurn(turnCtx, sessionID, metric)
		interrupted := turnCtx.Err() != nil
		s.cancelMu.Lock()
		if s.turnDone == turnDone {
			s.cancelTurn, s.runningSession, s.turnDone = nil, "", nil
		}
		s.cancelMu.Unlock()
		close(turnDone)
		cancel()
		s.hub.publish(sessionID, agent.Event{Kind: agent.EventInfo}, map[string]any{
			"backgroundContinuation": "finished", "succeeded": runErr == nil && !interrupted && mayContinue,
		})
		s.runMu.Unlock()
		if runErr != nil || interrupted || !mayContinue {
			if runErr != nil && !interrupted {
				log.Printf("background continuation for %s stopped: %v", sessionID, runErr)
			}
			return
		}
		// Recheck the queue before waiting: another job may have finished
		// after Loop's final drain, even if its wakeup was already coalesced.
	}
}
