package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

type backgroundTestProvider struct {
	fakeProvider
	requests chan llm.Request
}

func (p *backgroundTestProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.requests <- req
	answer := "WAITING_FOR_BACKGROUND"
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, "<job_notification>") {
				answer = "AUTOMATIC_CONTINUATION_DONE"
			}
		}
	}
	return &backgroundTestStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: answer},
		{Type: "message_stop", StopReason: "end_turn"},
	}}, nil
}

type backgroundTestStream struct{ events []llm.StreamEvent }

func (s *backgroundTestStream) Recv() (llm.StreamEvent, error) {
	if len(s.events) == 0 {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (*backgroundTestStream) Close() error { return nil }

func newBackgroundTestModel(t *testing.T) (*Model, *backgroundTestProvider) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	m, _ := newSessionSwitchModel(t, permission.ModeBypassPermissions)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	m.ctx = ctx
	p := &backgroundTestProvider{requests: make(chan llm.Request, 8)}
	m.loop.Provider = p
	m.loop.Jobs = jobs.NewRegistry(t.TempDir())
	m.loop.JobNotify = m.loop.Jobs.Notify()
	t.Cleanup(func() { m.loop.Jobs.ResetAndWait(0) })
	return m, p
}

// Hold a real subprocess on stdin until the test releases it. No sleeps,
// network, credentials or provider billing are involved.
func backgroundTestJob(t *testing.T, pool *jobs.Registry) func() {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh required for subprocess fixture")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close(); writer.Close() })
	cmd := exec.Command(shell, "-c", "read line; printf BACKGROUND_OUTPUT")
	cmd.Stdin = reader
	_, err = pool.Spawn(jobs.SpawnArgs{Command: "local completion fixture", Cmd: cmd})
	if err != nil {
		t.Fatal(err)
	}
	return func() { writer.Close() }
}

func finishBackgroundTestTurn(t *testing.T, m *Model) {
	t.Helper()
	for {
		select {
		case event := <-m.eventCh:
			m.handleAgentEvent(event)
		case err := <-m.doneCh:
			m.drainAgentEvents(len(m.eventCh))
			m.finalizeTurn(err)
			if err != nil {
				t.Fatalf("Run failed: %v", err)
			}
			return
		case <-m.ctx.Done():
			t.Fatal("turn did not finish")
		}
	}
}

func TestBackgroundCompletionResumesIdleTUIWithoutUserInput(t *testing.T) {
	m, p := newBackgroundTestModel(t)
	release := backgroundTestJob(t, m.loop.Jobs)
	wait := m.waitForBackgroundJob()
	m.beginTurn("Report the background result when it finishes")
	finishBackgroundTestTurn(t, m)
	if len(p.requests) != 1 {
		t.Fatal("a running ordinary background job must not hold the first turn open")
	}
	m.input.SetValue("unsent draft [Image #1]")
	m.imagePaste = map[int]string{1: "unsent-image.png"}
	modal := screen.NewBodyScreen("/agents", "open screen")
	m.activeScreen = modal
	release()
	ready := wait()
	m.Update(ready) // lifecycle wake, no key or fresh prompt
	if !m.turnActive {
		t.Fatal("completion failed to start automatic continuation")
	}
	finishBackgroundTestTurn(t, m)
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial + continuation", len(p.requests))
	}
	if m.input.Value() != "unsent draft [Image #1]" || m.imagePaste[1] != "unsent-image.png" || m.activeScreen != modal {
		t.Fatal("automatic continuation changed pending user input or the open screen")
	}
	if m.loop.Jobs.HasPendingNotifications() {
		t.Fatal("continuation did not consume notification")
	}
	if cmd := m.resumeForBackgroundJob(); cmd != nil {
		t.Fatal("stale readiness triggered duplicate Run")
	}
	var users, completions int
	for _, msg := range m.messages {
		if msg.Role == "user" {
			users++
		}
		if msg.Role == "assistant" && strings.Contains(msg.Content, "AUTOMATIC_CONTINUATION_DONE") {
			completions++
		}
	}
	if users != 1 || completions != 1 {
		t.Fatalf("display users=%d completions=%d, want 1 each", users, completions)
	}
	_, history, err := m.session.Load(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var injections, finals int
	for _, msg := range history {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, "<job_notification>") {
				injections++
			}
			if strings.Contains(block.Text, "AUTOMATIC_CONTINUATION_DONE") {
				finals++
			}
		}
	}
	if injections != 1 || finals != 1 {
		t.Fatalf("durable injections=%d finals=%d, want 1 each", injections, finals)
	}
}

func TestBackgroundCompletionAtTurnExitIsNotLost(t *testing.T) {
	m, p := newBackgroundTestModel(t)
	release := backgroundTestJob(t, m.loop.Jobs)
	m.beginTurn("wait for completion")
	finishBackgroundTestTurn(t, m)
	// Simulate a finished Run whose doneCh has not yet been handled by Update.
	m.turnActive = true
	release()
	m.Update(m.waitForBackgroundJob()())
	if len(p.requests) != 1 {
		t.Fatal("wake started overlapping Run")
	}
	m.finalizeTurn(nil)
	m.Update(spinnerTick{})
	if !m.turnActive {
		t.Fatal("finalization lost pending completion wake")
	}
	finishBackgroundTestTurn(t, m)
	if len(p.requests) != 2 {
		t.Fatalf("provider calls=%d, want 2", len(p.requests))
	}
}

func TestBackgroundResumeBoundaries(t *testing.T) {
	for _, boundary := range []string{"cancel", "error", "deadline", "queue", "rewind", "permission", "closed", "context", "reset", "disabled", "hook", "unknown_stop"} {
		t.Run(boundary, func(t *testing.T) {
			m, p := newBackgroundTestModel(t)
			release := backgroundTestJob(t, m.loop.Jobs)
			m.backgroundResumeAllowed = true
			release()
			m.waitForBackgroundJob()() // retain envelope, consume only readiness
			switch boundary {
			case "cancel":
				m.turnCancelledByUser = true
				m.finalizeTurn(nil)
			case "error":
				m.finalizeTurn(errors.New("provider unavailable"))
			case "deadline":
				m.finalizeTurn(context.DeadlineExceeded)
			case "queue":
				m.queuePending = true
			case "rewind":
				m.rewindSummaryPending = true
			case "permission":
				m.permissionModePending = true
			case "hook":
				m.handleAgentEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "halted_by_hook"})
			case "unknown_stop":
				m.handleAgentEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "unknown"})
			case "closed":
				m.sessionBoundaryClosed = true
			case "context":
				ctx, cancel := context.WithCancel(m.ctx)
				cancel()
				m.ctx = ctx
			case "reset":
				m.loop.Jobs.ResetAndWait(0)
			case "disabled":
				m.backgroundResumeAllowed = false
			}
			if m.resumeForBackgroundJob() != nil || m.turnActive || len(p.requests) != 0 {
				t.Fatal("background completion crossed stop/session/input boundary")
			}
		})
	}
}

func TestBackgroundWaiterStopsWithUIContext(t *testing.T) {
	m, _ := newBackgroundTestModel(t)
	ctx, cancel := context.WithCancel(m.ctx)
	m.ctx = ctx
	wait := m.waitForBackgroundJob()
	cancel()
	if msg := wait(); msg != nil {
		t.Fatalf("cancelled waiter returned %T", msg)
	}
}

// Observe active -> idle only on Bubble Tea's own Update goroutine. A rendered
// text delta is not proof that the initial Run and finalizeTurn have finished.
// Keeping this wrapper as the returned model preserves the observation across
// updates without production hooks or test-driven event-loop ticks.
type backgroundIdleProbeModel struct {
	*Model
	seenActive bool
	firstIdle  chan struct{}
}

func (m *backgroundIdleProbeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, cmd := m.Model.Update(msg)
	m.Model = updated.(*Model)
	if m.turnActive {
		m.seenActive = true
	} else if m.seenActive && m.firstIdle != nil {
		close(m.firstIdle)
		m.firstIdle = nil
	}
	return m, cmd
}

// Exercise the actual Bubble Tea event/command scheduler and renderer. Once
// the first prompt is submitted, there are no more key presses or test-driven
// spinner ticks: the process completion alone must wake the idle UI.
func TestBackgroundCompletionWakesRealTeaProgram(t *testing.T) {
	m, p := newBackgroundTestModel(t)
	release := backgroundTestJob(t, m.loop.Jobs)
	m.input.SetValue("Report the background result when it finishes")
	firstIdle := make(chan struct{})
	probe := &backgroundIdleProbeModel{Model: m, firstIdle: firstIdle}
	output := &lockedOutput{}
	program := tea.NewProgram(probe,
		tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(output),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor"}),
		tea.WithoutSignals(), tea.WithWindowSize(100, 40),
	)
	runDone := make(chan error, 1)
	go func() {
		_, err := program.Run()
		runDone <- err
	}()
	t.Cleanup(func() {
		program.Quit()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			program.Kill()
		}
	})
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case <-firstIdle:
	case <-time.After(5 * time.Second):
		t.Fatal("initial turn did not finalize in the real Bubble Tea event loop")
	}
	if len(p.requests) != 1 {
		t.Fatalf("provider calls before releasing background job=%d, want 1", len(p.requests))
	}
	waitForRendererOutput(t, output, 5*time.Second, func(s string) bool {
		return strings.Contains(s, "WAITING_FOR_BACKGROUND")
	})
	release()
	waitForRendererOutput(t, output, 5*time.Second, func(s string) bool {
		return strings.Contains(s, "AUTOMATIC_CONTINUATION_DONE")
	})
	if len(p.requests) != 2 {
		t.Fatalf("provider calls=%d, want exactly one completion continuation", len(p.requests))
	}
}
