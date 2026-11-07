package tui

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type wakeupReceiptProvider struct {
	fakeProvider
	answer string
	err    error
}

func (p wakeupReceiptProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	if p.err != nil {
		return nil, p.err
	}
	events := []llm.StreamEvent{{Type: "message_stop", StopReason: "end_turn"}}
	if p.answer != "" {
		events = append([]llm.StreamEvent{{Type: "text_delta", TextDelta: p.answer}}, events...)
	}
	return &backgroundTestStream{events: events}, nil
}

func wakeupReceiptModel(t *testing.T) *Model {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")
	t.Chdir(t.TempDir())
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	m.providerName = "fake"
	if err := store.WriteHeaderFull(session.Header{ID: m.sessionID, WorkDir: dir, Provider: m.providerName, Model: m.model}); err != nil {
		t.Fatal(err)
	}
	m.loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "an earlier unrelated request"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "PRIOR_UNRELATED_SUCCESS"}}},
	}
	m.cronSvc, err = agent.NewCronService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func runWakeupReceiptTurn(t *testing.T, m *Model, provider wakeupReceiptProvider, cancelled bool, persistErr error, beforeFinish ...func(*Model)) *agent.CronRunRecord {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	job := &agent.CronJob{
		Kind: agent.CronKindContinuation, OriginSessionID: m.sessionID,
		WorkDir: dir, Provider: m.providerName, Model: m.model,
		Prompt: "run a fresh scheduled check", Enabled: true,
		Schedule: agent.CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339Nano)},
	}
	if err := m.cronSvc.Create(job); err != nil {
		t.Fatal(err)
	}
	m.loop.Provider = provider
	if cmd := m.consumeScheduledWakeup(time.Now()); cmd == nil || m.wakeupRun == nil {
		t.Fatal("due continuation did not enter the original session")
	}
	runID := m.wakeupRun.Job.ContinuationRunID
	var runErr error
	select {
	case runErr = <-m.doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture turn did not finish")
	}
	m.backgroundResumeAllowed = runErr == nil
	m.turnCancelledByUser = cancelled
	if cancelled {
		runErr = context.Canceled
	}
	for _, changeHistory := range beforeFinish {
		changeHistory(m)
	}
	m.finishScheduledWakeup(runErr, persistErr)
	m.turnActive = false
	m.turnCancelledByUser = false
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	record, err := agent.ReadCronRun(m.cronSvc.Root(), job.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// Exercise actual admission and Loop.Run, including a provider rejection before
// the first text delta. The receipt must not borrow an earlier session answer.
func TestScheduledWakeupReceiptUsesOnlyCurrentTurnOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   wakeupReceiptProvider
		cancelled  bool
		persistErr error
		status     string
		output     string
	}{
		{name: "failure before text", provider: wakeupReceiptProvider{err: errors.New("HTTP 400 bad request: fixture rejected before first token")}, status: "failed"},
		{name: "cancelled before text", provider: wakeupReceiptProvider{err: context.Canceled}, cancelled: true, status: "cancelled"},
		{name: "successful new answer", provider: wakeupReceiptProvider{answer: "CURRENT_WAKEUP_ANSWER"}, status: "succeeded", output: "CURRENT_WAKEUP_ANSWER"},
		{name: "receipt after persistence failure", provider: wakeupReceiptProvider{answer: "CURRENT_UNSAVED_ANSWER"}, persistErr: errors.New("fixture persistence failure"), status: "failed", output: "CURRENT_UNSAVED_ANSWER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := wakeupReceiptModel(t)
			record := runWakeupReceiptTurn(t, m, tc.provider, tc.cancelled, tc.persistErr)
			if record.Status != tc.status || record.Output != tc.output {
				t.Fatalf("receipt status=%q output=%q, want status=%q output=%q", record.Status, record.Output, tc.status, tc.output)
			}
		})
	}
}

func TestScheduledWakeupReceiptDoesNotReusePreviousWakeupOutput(t *testing.T) {
	m := wakeupReceiptModel(t)
	first := runWakeupReceiptTurn(t, m, wakeupReceiptProvider{answer: "FIRST_WAKEUP_ANSWER"}, false, nil)
	if first.Status != "succeeded" || first.Output != "FIRST_WAKEUP_ANSWER" {
		t.Fatalf("first receipt: %+v", first)
	}
	second := runWakeupReceiptTurn(t, m, wakeupReceiptProvider{err: errors.New("HTTP 400 bad request: second wakeup rejected")}, false, nil)
	if second.Status != "failed" || second.Output != "" {
		t.Fatalf("second receipt borrowed previous output: status=%q output=%q", second.Status, second.Output)
	}
}

func TestScheduledWakeupReceiptDoesNotFallBackAfterHistoryReplacement(t *testing.T) {
	m := wakeupReceiptModel(t)
	record := runWakeupReceiptTurn(t, m, wakeupReceiptProvider{answer: "CURRENT_WAKEUP_ANSWER"}, false, nil, func(m *Model) {
		// Compaction/restore may replace the logical history before the host
		// writes its receipt. A vanished index cannot authorize an older answer.
		m.loop.ResetSession([]llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "REPLACED_UNRELATED_SUMMARY"}}}})
	})
	if record.Output != "" {
		t.Fatalf("invalidated wakeup boundary fell back to replacement history: %q", record.Output)
	}
}
