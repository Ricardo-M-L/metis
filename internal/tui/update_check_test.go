package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func TestUpdateCheckSubmitIsAsyncGuardedAndCancelable(t *testing.T) {
	m := newSlashTestModel(t)
	base, cancel := context.WithCancel(context.Background())
	m.ctx = base
	started := make(chan struct{}, 1)
	m.updateCheckRunner = func(ctx context.Context) (string, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > updateCheckTimeout {
			t.Errorf("update runner received no bounded deadline: %v %v", deadline, ok)
		}
		started <- struct{}{}
		<-ctx.Done()
		return "partial output", ctx.Err()
	}

	m.input.SetValue("/update")
	begin := time.Now()
	cmd := pressEnter(t, m)
	if elapsed := time.Since(begin); elapsed > 200*time.Millisecond {
		t.Fatalf("/update blocked the Bubble Tea submit path for %s", elapsed)
	}
	if cmd == nil || !m.updateCheckPending {
		t.Fatalf("/update did not return an async command/pending guard: cmd=%v pending=%v", cmd, m.updateCheckPending)
	}
	select {
	case <-started:
		t.Fatal("update runner started synchronously inside handleSubmit")
	default:
	}
	if got := m.messages[len(m.messages)-1].Content; !strings.Contains(got, "checking") {
		t.Fatalf("immediate update status = %q", got)
	}

	m.input.SetValue("/update")
	if duplicate := pressEnter(t, m); duplicate != nil {
		t.Fatal("duplicate /update started another command")
	}
	if got := m.messages[len(m.messages)-1].Content; !strings.Contains(got, "already in progress") {
		t.Fatalf("duplicate update status = %q", got)
	}

	resultCh := make(chan updateCheckResultMsg, 1)
	go func() { resultCh <- cmd().(updateCheckResultMsg) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async update runner did not start")
	}
	cancel()
	var result updateCheckResultMsg
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("canceled update runner did not finish")
	}
	_, _ = m.Update(result)
	if m.updateCheckPending {
		t.Fatal("canceled update left pending guard set")
	}
	got := m.messages[len(m.messages)-1]
	if got.Role != "warning" || !strings.Contains(got.Content, "partial output") || !strings.Contains(got.Content, "canceled") {
		t.Fatalf("canceled update result = %+v", got)
	}
}

func TestUpdateCheckCompletionOpensBodyAndPreservesExitErrors(t *testing.T) {
	m := newSlashTestModel(t)
	m.updateCheckRunner = func(context.Context) (string, error) {
		return "Metis is current", nil
	}
	m.input.SetValue("/update")
	msg := runCmd(t, pressEnter(t, m))
	_, _ = m.Update(msg)
	if _, ok := m.activeScreen.(*screen.BodyScreen); !ok || !strings.Contains(m.activeScreen.View(), "Metis is current") {
		t.Fatalf("successful update did not open result body: %T %q", m.activeScreen, m.activeScreen.View())
	}

	formatted := formatUpdateCheckResult("stderr detail", errors.New("exit status 7"))
	if !strings.Contains(formatted, "exit status 7") || !strings.Contains(formatted, "stderr detail") {
		t.Fatalf("plain update formatting discarded exit error/output: %q", formatted)
	}
}
