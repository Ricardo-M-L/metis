package webui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type wakeupTestRunner struct {
	fail     bool
	sessions []string
	requests []IsolatedTurnRequest
}

func (*wakeupTestRunner) Eligible(*session.Header) bool { return true }
func (r *wakeupTestRunner) Run(_ context.Context, req IsolatedTurnRequest) (IsolatedTurnResult, error) {
	r.sessions = append(r.sessions, req.SessionID)
	r.requests = append(r.requests, req)
	if r.fail {
		return IsolatedTurnResult{}, errors.New("fixture failure")
	}
	return IsolatedTurnResult{Text: "wakeup answer"}, nil
}

func TestScheduledWakeupFormalDesktopOriginalSessionAndReceipt(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			root := t.TempDir()
			svc, _ := agent.NewCronService(root)
			store, _ := session.NewStore(t.TempDir())
			dir := t.TempDir()
			for _, id := range []string{"original", "viewed"} {
				if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: dir, Provider: "fake", Model: "model", Mode: string(permission.ModeAsk)}); err != nil {
					t.Fatal(err)
				}
			}
			runner := &wakeupTestRunner{fail: fail}
			s := NewServer("", agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2), store, RuntimeBindings{InitialSessionID: "original", CronService: svc, IsolatedRunner: runner})
			s.stateMu.Lock()
			s.activeSessionID = "viewed"
			s.stateMu.Unlock()
			now := time.Now()
			job := &agent.CronJob{Kind: agent.CronKindContinuation, OriginSessionID: "original", Provider: "fake", Model: "model", WorkDir: dir, Prompt: "continue original", Enabled: true, Schedule: agent.CronSchedule{Kind: "at", At: now.Add(time.Second).Format(time.RFC3339Nano)}}
			if err := svc.Create(job); err != nil {
				t.Fatal(err)
			}
			s.consumeScheduledWakeups(context.Background(), now)
			if len(runner.sessions) != 0 {
				t.Fatal("Create fired before due")
			}
			pending := s.scheduledWakeupStatus("original", now.Add(2*time.Second))
			if len(pending) != 1 || pending[0]["status"] != "overdue" {
				t.Fatalf("overdue absent: %v", pending)
			}
			s.consumeScheduledWakeups(context.Background(), now.Add(2*time.Second))
			if len(runner.sessions) != 1 || runner.sessions[0] != "original" {
				t.Fatalf("wrong session: %v", runner.sessions)
			}
			records, err := agent.ListCronRuns(root, job.ID, 10)
			want := "succeeded"
			if fail {
				want = "failed"
			}
			if err != nil || len(records) != 1 || records[0].SessionID != "original" || records[0].Status != want {
				t.Fatalf("receipt: %+v %v", records, err)
			}
			terminal := s.scheduledWakeupStatus("original", now.Add(3*time.Second))
			if len(terminal) != 1 || terminal[0]["status"] != want {
				t.Fatalf("terminal receipt display: %v", terminal)
			}
			if !runner.requests[0].Resume || runner.requests[0].Input != "" {
				t.Fatal("scheduled worker must consume durable internal context, not user input")
			}
			_, history, _ := store.Load("original")
			if len(history) != 1 || len(history[0].Content) == 0 || !history[0].Content[0].Synthetic {
				t.Fatalf("scheduled input impersonated human: %+v", history)
			}
			found := false
			s.hub.mu.RLock()
			for _, ev := range s.hub.replay {
				if ev.ev.Info == "scheduled wakeup "+want {
					found = true
					if ev.session != "original" {
						t.Fatal("notification crossed sessions")
					}
				}
			}
			s.hub.mu.RUnlock()
			if !found {
				t.Fatal("missing terminal notification")
			}
		})
	}
}
