package tui

import (
	"context"
	"os"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestWakeupCreationRejectsRestoredForeignWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	original := t.TempDir()
	_ = store.WriteHeaderFull(session.Header{ID: m.sessionID, WorkDir: original, Provider: "fake", Model: m.model})
	svc, err := agent.NewCronService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.cronSvc = svc
	m.providerName = "fake"
	m.loop.Registry.Register(builtin.NewScheduleWakeup(m.gate, svc))
	m.bindWakeupContinuation()
	tool, _ := m.loop.Registry.Get("ScheduleWakeup")
	result, err := tool.Execute(tasks.WithSessionID(context.Background(), m.sessionID), map[string]any{"delaySeconds": 30, "prompt": "continue", "reason": "wrong workspace"})
	if err != nil || !result.IsError || len(svc.List()) != 0 {
		t.Fatalf("reported unusable cross-workspace wakeup success: %+v %v", result, err)
	}
	// Once the actual tool cwd is restored, exactly the same registry entry works.
	if err := os.Chdir(original); err != nil {
		t.Fatal(err)
	}
	m.bindWakeupContinuation()
	tool, _ = m.loop.Registry.Get("ScheduleWakeup")
	result, err = tool.Execute(tasks.WithSessionID(context.Background(), m.sessionID), map[string]any{"delaySeconds": 30, "prompt": "continue", "reason": "correct workspace"})
	if err != nil || result.IsError || len(svc.List()) != 1 {
		t.Fatalf("restored workspace not accepted: %+v %v", result, err)
	}
}
