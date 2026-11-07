package tui

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestScheduledWakeupTUITurnBindsVisibleToolCreatesThenConsumes(t *testing.T) {
	t.Chdir(t.TempDir())
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	dir, _ := os.Getwd()
	m.providerName = "fake"
	_ = store.WriteHeaderFull(session.Header{ID: m.sessionID, WorkDir: dir, Provider: "fake", Model: m.model})
	root := t.TempDir()
	svc, _ := agent.NewCronService(root)
	m.cronSvc = svc
	m.loop.Registry.Register(builtin.NewScheduleWakeup(m.gate, svc))
	m.loop.Provider = &backgroundTestProvider{requests: make(chan llm.Request, 4)}
	m.startAgentTurn(context.Background())
	select {
	case <-m.doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("initial turn blocked")
	}
	m.turnActive = false
	tool, _ := m.loop.Registry.Get("ScheduleWakeup")
	result, err := tool.Execute(tasks.WithSessionID(context.Background(), m.sessionID), map[string]any{"delaySeconds": 30, "prompt": "continue", "reason": "entry fixture"})
	if err != nil || result.IsError {
		t.Fatalf("unbound tool: %+v %v", result, err)
	}
	jobs := svc.List()
	if len(jobs) != 1 {
		t.Fatal("tool failed to create")
	}
	job := jobs[0]
	if job.OriginSessionID != m.sessionID || job.WorkDir != dir || job.Provider != "fake" || job.Model != m.model {
		t.Fatalf("bad identity %+v", job)
	}
	if m.consumeScheduledWakeup(time.Now()) != nil {
		t.Fatal("ran before due")
	}
	if m.consumeScheduledWakeup(job.NextRun.Add(time.Second)) == nil {
		t.Fatal("did not consume")
	}
	select {
	case <-m.doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("wakeup blocked")
	}
	m.backgroundResumeAllowed = true
	m.finishScheduledWakeup(nil, nil)
	records, err := agent.ListCronRuns(root, job.ID, 10)
	if err != nil || len(records) != 1 || records[0].Status != "succeeded" {
		t.Fatalf("missing completion %+v %v", records, err)
	}
}
