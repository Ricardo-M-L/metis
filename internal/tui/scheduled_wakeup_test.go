package tui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestScheduledWakeupTUIBusyOriginalSessionAndOutcome(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			t.Chdir(t.TempDir())
			m.loop.Provider = &backgroundTestProvider{requests: make(chan llm.Request, 2)}
			dir, _ := os.Getwd()
			m.providerName = "fake"
			if err := store.WriteHeaderFull(session.Header{ID: "source", WorkDir: dir, Provider: "fake", Model: m.model}); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			svc, _ := agent.NewCronService(root)
			m.cronSvc = svc
			now := time.Now()
			job := &agent.CronJob{Kind: agent.CronKindContinuation, OriginSessionID: "source", WorkDir: dir, Provider: "fake", Model: m.model, Prompt: "continue source", Enabled: true, Schedule: agent.CronSchedule{Kind: "at", At: now.Add(time.Second).Format(time.RFC3339Nano)}}
			if err := svc.Create(job); err != nil {
				t.Fatal(err)
			}
			if cmd := m.consumeScheduledWakeup(now); cmd != nil {
				t.Fatal("created is not due")
			}
			m.turnActive = true
			m.consumeScheduledWakeup(now.Add(2 * time.Second))
			j, _ := svc.Get(job.ID)
			if j.RunCount != 0 || j.ContinuationStatus == "claimed" {
				t.Fatal("busy turn must leave wakeup durably queued")
			}
			found := false
			for _, msg := range m.messages {
				found = found || strings.Contains(msg.Content, "overdue")
			}
			if !found {
				t.Fatal("missing overdue display")
			}
			m.turnActive = false
			// Wrong session is never allowed to drain original work.
			m.sessionID = "viewed"
			m.consumeScheduledWakeup(now.Add(2 * time.Second))
			j, _ = svc.Get(job.ID)
			if j.RunCount != 0 {
				t.Fatal("cross-session wakeup")
			}
			m.sessionID = "source"
			if cmd := m.consumeScheduledWakeup(now.Add(2 * time.Second)); cmd == nil || m.wakeupRun == nil {
				t.Fatal("due wakeup did not start original turn")
			}
			select {
			case <-m.doneCh:
			case <-time.After(3 * time.Second):
				t.Fatal("turn did not complete")
			}
			_, history, _ := store.Load("source")
			if len(history) == 0 || len(history[0].Content) == 0 || !history[0].Content[0].Synthetic {
				t.Fatal("wakeup impersonated user input")
			}
			m.backgroundResumeAllowed = true
			var runErr error
			if fail {
				runErr = errors.New("fixture error")
			}
			m.finishScheduledWakeup(runErr, nil)
			records, err := agent.ListCronRuns(root, job.ID, 10)
			want := "succeeded"
			if fail {
				want = "failed"
			}
			if err != nil || len(records) != 1 || records[0].SessionID != "source" || records[0].Status != want {
				t.Fatalf("receipt %+v %v", records, err)
			}
			if got := m.messages[len(m.messages)-1].Content; got != "scheduled wakeup "+want {
				t.Fatal(got)
			}
			if m.turnCancel != nil {
				m.turnCancel()
			}
		})
	}
}
