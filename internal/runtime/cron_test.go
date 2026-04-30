package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func newCronTestCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Session.Dir = dir
	return cfg
}

func TestCronChatService_RenderEmpty(t *testing.T) {
	svc, err := NewCronChatService(newCronTestCfg(t))
	if err != nil {
		t.Fatalf("NewCronChatService: %v", err)
	}
	out := svc.RenderList()
	if !strings.Contains(out, "no cron jobs") {
		t.Errorf("empty render should hint no jobs; got %q", out)
	}
}

func TestCronChatService_AddListRoundTrip(t *testing.T) {
	svc, err := NewCronChatService(newCronTestCfg(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id, err := svc.Add("5m", "summarize the day")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id == "" {
		t.Fatal("Add should return a non-empty id")
	}
	out := svc.RenderList()
	if !strings.Contains(out, id) {
		t.Errorf("RenderList missing the new job id: %s", out)
	}
	if !strings.Contains(out, "summarize the day") {
		t.Errorf("RenderList missing the prompt: %s", out)
	}
}

func TestCronChatService_RejectsTooShortInterval(t *testing.T) {
	svc, _ := NewCronChatService(newCronTestCfg(t))
	if _, err := svc.Add("5s", "p"); err == nil {
		t.Error("5s should be rejected as too short")
	}
}

func TestCronChatService_RejectsBadInterval(t *testing.T) {
	svc, _ := NewCronChatService(newCronTestCfg(t))
	if _, err := svc.Add("not-a-duration", "p"); err == nil {
		t.Error("non-duration should be rejected")
	}
}

func TestCronChatService_RejectsEmptyInputs(t *testing.T) {
	svc, _ := NewCronChatService(newCronTestCfg(t))
	if _, err := svc.Add("", "p"); err == nil {
		t.Error("empty interval should error")
	}
	if _, err := svc.Add("5m", ""); err == nil {
		t.Error("empty prompt should error")
	}
}

func TestCronChatService_Remove(t *testing.T) {
	svc, _ := NewCronChatService(newCronTestCfg(t))
	id, _ := svc.Add("5m", "x")
	if err := svc.Remove(id); err != nil {
		t.Errorf("Remove existing: %v", err)
	}
	if err := svc.Remove("nonexistent"); err == nil {
		t.Error("Remove of unknown id should error with 'no job with id'")
	}
}

func TestCronChatService_PauseResume(t *testing.T) {
	svc, _ := NewCronChatService(newCronTestCfg(t))
	id, _ := svc.Add("5m", "p")
	if err := svc.Pause(id); err != nil {
		t.Errorf("Pause: %v", err)
	}
	out := svc.RenderList()
	if !strings.Contains(out, "paused") {
		t.Errorf("after Pause, render should show paused state: %s", out)
	}
	if err := svc.Resume(id); err != nil {
		t.Errorf("Resume: %v", err)
	}
	out = svc.RenderList()
	if strings.Contains(out, "paused") {
		t.Errorf("after Resume, render should not show paused: %s", out)
	}
}
