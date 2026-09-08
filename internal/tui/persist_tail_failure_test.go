package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// Fail the real append operation without destroying the already durable
// prefix. Restoring the same file models a transient filesystem failure.
func temporarilyBlockSessionTail(t *testing.T, store *session.Store, id string) func() {
	t.Helper()
	path := filepath.Join(store.Dir, id+".jsonl")
	saved := path + ".before-failure"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		t.Helper()
		if restored {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(saved, path); err != nil {
			t.Fatal(err)
		}
		restored = true
	}
	t.Cleanup(restore)
	return restore
}

func completedTailHistory() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Preserve the finished task."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "COMPLETED_TRANSCRIPT_MUST_SURVIVE"}}},
	}
}

func assertStoredTail(t *testing.T, store *session.Store, id string, want []llm.Message) {
	t.Helper()
	_, got, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable history = %#v, want %#v", got, want)
	}
}

func TestCompletedTurnTailRetriesAtTUIBoundary(t *testing.T) {
	for _, boundary := range []string{"close", "session_switch"} {
		t.Run(boundary, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			want := completedTailHistory()
			m.loop.Restore(want)
			restore := temporarilyBlockSessionTail(t, store, "source")
			m.finalizeTurn(nil)
			if m.turnActive {
				t.Fatal("fixture must exercise an already completed turn")
			}
			restore()
			switch boundary {
			case "close":
				if err := m.stopForegroundTurnForClose(); err != nil {
					t.Fatal(err)
				}
				if err := m.leaveActiveSession("cli-close", true); err != nil {
					t.Fatal(err)
				}
				// The shutdown and memory boundaries can both retry the same
				// cursor; doing so must not duplicate the completed exchange.
				if err := m.stopForegroundTurnForClose(); err != nil {
					t.Fatal(err)
				}
			case "session_switch":
				target := &session.Header{ID: "target", Model: "test-model", System: "target-system"}
				if err := store.WriteHeaderFull(*target); err != nil {
					t.Fatal(err)
				}
				if err := m.activateSession(target.ID, target, nil, true); err != nil {
					t.Fatal(err)
				}
			}
			assertStoredTail(t, store, "source", want)
		})
	}
}

func TestCompletedTurnTailFailureIsVisibleAndRevokesBackgroundResume(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.Restore(completedTailHistory())
	m.backgroundResumeAllowed = true
	m.queuedPrompts = []queuedItem{{Text: "Keep this queued until storage is safe."}}
	temporarilyBlockSessionTail(t, store, "source")
	m.finalizeTurn(nil)
	found := false
	for _, message := range m.messages {
		if message.Role == "warning" && strings.Contains(message.Content, "save session") {
			found = true
		}
	}
	if !found {
		t.Errorf("completed turn swallowed its transcript write failure: %+v", m.messages)
	}
	if m.backgroundResumeAllowed {
		t.Error("failed transcript save still permits automatic background continuation")
	}
	if m.queuePending || len(m.queuedPrompts) != 1 {
		t.Errorf("failed transcript save drained the queued prompt: pending=%v queued=%+v", m.queuePending, m.queuedPrompts)
	}
}

func TestCompletedTurnTUIBoundaryRefusesUnwritableTail(t *testing.T) {
	for _, boundary := range []string{"close", "session_switch"} {
		t.Run(boundary, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			repository := attachLifecycleMemory(t, m.loop)
			want := completedTailHistory()
			m.loop.Restore(want)
			temporarilyBlockSessionTail(t, store, "source")
			m.finalizeTurn(nil)
			var err error
			if boundary == "close" {
				err = m.stopForegroundTurnForClose()
			} else {
				target := &session.Header{ID: "target", Model: "test-model"}
				if writeErr := store.WriteHeaderFull(*target); writeErr != nil {
					t.Fatal(writeErr)
				}
				err = m.activateSession(target.ID, target, nil, true)
			}
			if err == nil {
				t.Fatal("boundary succeeded without persisting the completed tail")
			}
			if m.sessionID != "source" || m.sessionBoundaryClosed || !reflect.DeepEqual(m.loop.History(), want) {
				t.Fatalf("failed boundary lost the live source: id=%q closed=%v history=%#v", m.sessionID, m.sessionBoundaryClosed, m.loop.History())
			}
			notes, err := repository.ListDailyNotes(20)
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) != 0 {
				t.Fatalf("failed transcript boundary wrote a clean Daily note: %+v", notes)
			}
		})
	}
}

func TestCompletedTurnTailRetriesAtREPLBoundary(t *testing.T) {
	for _, boundary := range []string{"close", "new", "branch"} {
		t.Run(boundary, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
			if err != nil {
				t.Fatal(err)
			}
			r.out = &bytes.Buffer{}
			want := completedTailHistory()
			r.Loop.Restore(want)
			restore := temporarilyBlockSessionTail(t, store, "source")
			r.persistTail()
			restore()
			switch boundary {
			case "close":
				r.stdin = strings.NewReader("/quit\n")
				err = r.Run(context.Background())
			case "new":
				_, err = r.startFreshSession()
			case "branch":
				_, err = r.branchSession()
			}
			if err != nil {
				t.Fatal(err)
			}
			assertStoredTail(t, store, "source", want)
		})
	}
}

func TestREPLCompletedTurnReturnsSessionSaveFailure(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.Provider = &backgroundTestProvider{requests: make(chan llm.Request, 1)}
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	r.out = &bytes.Buffer{}
	r.Loop.AppendUser("Finish this task without discarding its transcript.")
	restore := temporarilyBlockSessionTail(t, store, "source")
	if err := r.runTurn(context.Background()); err == nil || !strings.Contains(err.Error(), "save session") {
		t.Fatalf("completed turn error = %v, want session persistence error", err)
	}
	want := r.Loop.History()
	if len(want) != 2 || want[1].Role != llm.RoleAssistant {
		t.Fatalf("completed answer was not retained: %#v", want)
	}
	restore()
	if err := r.leaveActiveSession("cli-close", true); err != nil {
		t.Fatal(err)
	}
	assertStoredTail(t, store, "source", want)
}

func TestTailRetryPreservesAlreadyWrittenPrefix(t *testing.T) {
	for _, frontend := range []string{"tui", "repl"} {
		t.Run(frontend, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
			if err != nil {
				t.Fatal(err)
			}
			broken := completedTailHistory()
			// JSON encoding fails on the second line only, after the first
			// message was appended successfully. No mock store/cursor is used.
			broken[1].Content[0].ToolInput = map[string]any{"invalid": make(chan struct{})}
			m.loop.Restore(broken)
			if frontend == "tui" {
				err = m.persistTail()
			} else {
				err = r.persistTail()
			}
			if err == nil {
				t.Fatal("fixture did not fail the second transcript write")
			}
			assertStoredTail(t, store, "source", broken[:1])
			want := completedTailHistory()
			m.loop.Restore(want)
			if frontend == "tui" {
				err = m.stopForegroundTurnForClose()
			} else {
				err = r.leaveActiveSession("cli-close", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertStoredTail(t, store, "source", want)
		})
	}
}

type tailCanceledProvider struct{ fakeProvider }

func (tailCanceledProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, context.Canceled
}

func TestREPLCancellationKeepsSessionSaveError(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.Provider = tailCanceledProvider{}
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	r.out = &bytes.Buffer{}
	r.Loop.AppendUser("Keep even this interrupted prompt.")
	temporarilyBlockSessionTail(t, store, "source")
	err = r.runTurn(context.Background())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "save session") {
		t.Fatalf("Run error = %v, want both cancellation and session persistence failure", err)
	}
}

func TestPromptSaveFailureWarnsBeforeRunningAndAllowsManualRecovery(t *testing.T) {
	for _, entry := range []string{"submit", "scheduled"} {
		t.Run(entry, func(t *testing.T) {
			m, provider := newBackgroundTestModel(t)
			restore := temporarilyBlockSessionTail(t, m.session, "source")
			if entry == "submit" {
				m.input.SetValue("Run this explicitly submitted task.")
				m.handleSubmit()
			} else {
				m.beginTurn("Run this scheduled task.")
			}
			found := false
			for _, message := range m.messages {
				found = found || (message.Role == "warning" && strings.Contains(message.Content, "save session"))
			}
			if !found {
				t.Error("initial prompt persistence failure was not visible")
			}
			// The requested task still runs; only unattended continuation is
			// revoked. Do not claim the request was blocked after sending it.
			finishBackgroundTestTurn(t, m)
			if len(provider.requests) != 1 || m.backgroundResumeAllowed {
				t.Fatalf("run/continuation status = %d/%v", len(provider.requests), m.backgroundResumeAllowed)
			}
			restore()
			m.beginTurn("Storage is restored; continue this task.")
			finishBackgroundTestTurn(t, m)
			if len(provider.requests) != 2 || !m.backgroundResumeAllowed {
				t.Fatal("explicit recovery did not restore normal continuation")
			}
			assertStoredTail(t, m.session, "source", m.loop.History())
		})
	}
}

func TestCompletedTurnTailRetrySameSessionResumeKeepsRecoveredHistory(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	want := completedTailHistory()
	m.loop.Restore(want)
	restore := temporarilyBlockSessionTail(t, store, "source")
	m.finalizeTurn(nil)
	restore()
	// This is the picker's ordering: it reads the destination before the
	// activation boundary can retry the still-unsaved source transcript.
	header, stale, err := store.Load("source")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatal("fixture unexpectedly persisted the completed turn")
	}
	if err := m.activateSession("source", header, stale, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.loop.History(), want) {
		t.Fatalf("same-session resume replaced recovered live history: %#v", m.loop.History())
	}
	assertStoredTail(t, store, "source", want)
	// Cursor rebind must describe the recovered snapshot, not the stale load.
	m.loop.AppendUser("next prompt after same-session resume")
	if err := m.stopForegroundTurnForClose(); err != nil {
		t.Fatal(err)
	}
	assertStoredTail(t, store, "source", m.loop.History())
}

func TestRecallPersistenceFailureIsNotLabeledAPIError(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	const private = "PRIVATE_STORAGE_PATH_AND_CONTENT"
	err := &memory.RecallPersistenceError{Err: errors.New(private)}
	m.streamingText = "COMPLETED_ANSWER"
	m.handleAgentEvent(agent.Event{Kind: agent.EventError, Err: err})
	m.finalizeTurn(err)
	var output strings.Builder
	errorsShown := 0
	for _, message := range m.messages {
		output.WriteString(message.Content)
		if message.Role == "error" {
			errorsShown++
		}
	}
	got := output.String()
	if !strings.Contains(got, "Memory save error:") || strings.Contains(got, "API Error:") {
		t.Fatalf("Recall failure mislabeled: %q", got)
	}
	if !strings.Contains(got, "COMPLETED_ANSWER") || strings.Contains(got, private) {
		t.Fatalf("answer lost or private storage detail exposed: %q", got)
	}
	if errorsShown != 1 {
		t.Fatalf("event/finalization emitted %d error rows, want one", errorsShown)
	}
}
