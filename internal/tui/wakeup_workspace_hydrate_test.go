package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestScheduledWakeupTUIRestoredWrongWorkspaceDoesNotClaim(t *testing.T) {
	origin, current := t.TempDir(), t.TempDir()
	t.Chdir(current)
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.providerName = "fake"
	_ = store.WriteHeaderFull(session.Header{ID: m.sessionID, WorkDir: origin, Provider: "fake", Model: m.model})
	svc, _ := agent.NewCronService(t.TempDir())
	m.cronSvc = svc
	job := &agent.CronJob{Kind: agent.CronKindContinuation, OriginSessionID: m.sessionID, WorkDir: origin, Provider: "fake", Model: m.model, Prompt: "do not run here", Enabled: true, Schedule: agent.CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339)}}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	if m.consumeScheduledWakeup(time.Now()) != nil {
		t.Fatal("executed original session in wrong workspace")
	}
	saved, _ := svc.Get(job.ID)
	if saved.RunCount != 0 || !saved.Enabled || saved.ContinuationStatus == "claimed" {
		t.Fatal("wrong workspace consumed pending job")
	}
	found := false
	for _, msg := range m.messages {
		found = found || strings.Contains(msg.Content, "original workspace differs")
	}
	if !found {
		t.Fatal("missing workspace warning")
	}
}
func TestScheduledWakeupTUIHydratesSyntheticAsInfo(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	m.messages = nil
	m.loop.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "human"}, {Type: "text", Text: "internal wakeup", Synthetic: true}}}}
	m.hydrateFromLoopHistory()
	roles := map[string]string{}
	for _, msg := range m.messages {
		roles[msg.Content] = msg.Role
	}
	if roles["human"] != "user" || roles["internal wakeup"] != "info" {
		t.Fatalf("synthetic input impersonates human: %v", roles)
	}
}
