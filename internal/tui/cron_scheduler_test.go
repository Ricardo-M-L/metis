package tui

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func TestCronTickIsHandledAndRearmedWhileFullScreenIsOpen(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	cronSvc, err := agent.NewCronService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.cronSvc = cronSvc
	modal := screen.NewBodyScreen("/test", "full-screen content")
	m.activeScreen = modal

	updated, cmd := m.Update(cronFireTickMsg(time.Now()))
	if updated != m {
		t.Fatalf("Update returned a different model: %T", updated)
	}
	if m.activeScreen != modal {
		t.Fatalf("scheduler tick changed the active screen: %T", m.activeScreen)
	}
	if cmd == nil {
		t.Fatal("scheduler tick was swallowed by active screen instead of re-arming")
	}
}
