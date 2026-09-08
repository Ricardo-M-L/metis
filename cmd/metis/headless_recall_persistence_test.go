package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func recallBoundaryLoop(repository memory.Repository, sessionID string) *agent.Loop {
	loop := agent.NewLoop(&headlessBoundaryProvider{}, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = repository
	loop.DistillEvery = 5
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: sessionID}
	}
	return loop
}

// Use the actual filesystem repository and Loop, not an EventError injected
// into the collector. Both daemon and coordinator use runHeadlessOneShot.
func TestHeadlessOneShotRejectsRecallStorageFailure(t *testing.T) {
	for _, target := range []string{"messages.jsonl", "sessions.json"} {
		t.Run(target, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "private-recall-location")
			manager, err := memory.NewMemoryManager(root)
			if err != nil {
				t.Fatal(err)
			}
			// An existing directory cannot be read as JSONL or replaced by the
			// atomic metadata rename, even when tests run with elevated rights.
			if err := os.Mkdir(filepath.Join(root, "recall", target), 0o700); err != nil {
				t.Fatal(err)
			}
			const sessionID = "completed-with-recall-failure"
			loop := recallBoundaryLoop(manager, sessionID)
			rt := &runtime{loop: loop, sessionID: sessionID}
			reportReader, reportWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reportReader.Close()
			defer reportWriter.Close()
			originalStderr := os.Stderr
			defer func() { os.Stderr = originalStderr }()
			os.Stderr = reportWriter
			text, err := runHeadlessOneShot(context.Background(), rt,
				"Remember the White Finch release codename.", "metis daemon task")
			os.Stderr = originalStderr
			if closeErr := reportWriter.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			actualReport, readErr := io.ReadAll(reportReader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err == nil {
				t.Fatal("headless task silently succeeded despite failed Recall RecordTurn")
			}
			if !strings.Contains(text, "White Finch") {
				t.Fatalf("completed task response lost: %q", text)
			}
			if strings.Contains(err.Error(), "private-recall-location") {
				t.Fatal("public recall failure exposed the private storage path")
			}
			encoded, marshalErr := json.Marshal(err)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var state map[string]string
			if decodeErr := json.Unmarshal(encoded, &state); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if state["task_status"] != "completed" || state["memory_status"] != "failed" || state["reason"] != "recall_persistence_failed" {
				t.Fatalf("missing structured recall failure status: %s", encoded)
			}
			var emitted map[string]string
			if decodeErr := json.Unmarshal(actualReport, &emitted); decodeErr != nil {
				t.Fatalf("actual daemon stderr did not contain structured status: %v", decodeErr)
			}
			if emitted["kind"] != "memory_persistence_error" || emitted["source"] != "daemon" || emitted["task_status"] != "completed" || emitted["memory_status"] != "failed" {
				t.Fatalf("incorrect actual daemon failure report: %s", actualReport)
			}
			if bytes.Contains(actualReport, []byte("private-recall-location")) {
				t.Fatal("structured report exposed the private storage path")
			}
			if pending := loop.FlushPendingDistillation(sessionID); pending != 0 {
				t.Fatalf("failed recall was queued for enrichment: %d", pending)
			}
		})
	}
}

func TestCollectHeadlessEventsPreservesRealRecallFailureEvent(t *testing.T) {
	root := t.TempDir()
	manager, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "recall", "sessions.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	loop := recallBoundaryLoop(manager, "event-only-recall-failure")
	loop.AppendUser("Remember the White Finch release codename.")
	var producerErr error
	text, err := collectHeadlessEvents(func(events chan<- agent.Event) error {
		producerErr = loop.Run(context.Background(), events)
		// Exercise the consumer's independent EventError contract, even if an
		// embedding wrapper consumes the producer's returned error itself.
		return nil
	})
	var persistenceErr *memory.RecallPersistenceError
	if !errors.As(producerErr, &persistenceErr) || !errors.As(err, &persistenceErr) {
		t.Fatalf("producer/event storage failure lost: producer=%v collector=%v", producerErr, err)
	}
	if !strings.Contains(text, "White Finch") {
		t.Fatal("event collector lost the completed answer")
	}
}

func TestExecuteCronRecallFailureJoinsProducerBeforeReturning(t *testing.T) {
	rt := newHeadlessOutcomeRuntime(t, &headlessBoundaryProvider{})
	if err := os.Mkdir(filepath.Join(rt.loop.Memory.Root(), "recall", "sessions.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHook)
	rt.loop.Hooks.Register(agent.SessionEndHandler(func(context.Context, agent.HookContext, int, string) {
		close(entered)
		<-release
		close(finished)
	}))
	job := &agent.CronJob{ID: "recall-failure-join", Prompt: "Remember the White Finch release codename.", Silent: true, SessionMode: agent.SessionModePersistent}
	history := map[string][]llm.Message{}
	done := make(chan error, 1)
	go func() {
		done <- executeCronJob(context.Background(), rt, job, history, map[string][]llm.Message{})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not reach the session-end defer")
	}
	select {
	case <-done:
		releaseHook()
		<-finished
		t.Fatal("cron returned on EventError before its producer/session-end defer finished")
	case <-time.After(30 * time.Millisecond):
	}
	releaseHook()
	select {
	case err := <-done:
		var persistenceErr *memory.RecallPersistenceError
		if !errors.As(err, &persistenceErr) {
			t.Fatalf("cron lost Recall persistence failure: %v", err)
		}
		if len(history) != 0 {
			t.Fatal("failed memory hand-off was published as successful cron history")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cron did not finish after the producer was released")
	}
}

type recallReportFailureWriter struct{ err error }

func (w recallReportFailureWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHeadlessRecallReportPreservesFailureAndSanitizesEverySource(t *testing.T) {
	storageErr := errors.New("private-storage-error")
	recallErr := &memory.RecallPersistenceError{Err: storageErr}
	for source, expected := range map[string]string{
		"metis run": "run", "metis mcp-serve run_task": "mcp", "cron job private-job": "cron",
		"metis daemon task": "daemon", "metis coordinator private-task": "coordinator", "private-label": "headless",
	} {
		t.Run(expected, func(t *testing.T) {
			var output bytes.Buffer
			err := reportHeadlessRecallFailure(recallErr, source, &output)
			if !errors.Is(err, storageErr) {
				t.Fatal("reporting lost the original storage failure")
			}
			var report map[string]string
			if err := json.Unmarshal(output.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report["source"] != expected || report["task_status"] != "completed" || report["memory_status"] != "failed" {
				t.Fatalf("unexpected report: %s", output.Bytes())
			}
			if strings.Contains(output.String(), "private-") {
				t.Fatal("report leaked untrusted labels or storage error text")
			}
		})
	}
	writerErr := errors.New("private-writer-error")
	err := reportHeadlessRecallFailure(recallErr, "metis run", recallReportFailureWriter{writerErr})
	if !errors.Is(err, storageErr) || !errors.Is(err, writerErr) {
		t.Fatal("writer failure must retain both the storage and reporting causes")
	}
	if strings.Contains(err.Error(), "private-") {
		t.Fatal("report failure leaked a raw error")
	}
	var output bytes.Buffer
	if got := reportHeadlessRecallFailure(memory.ErrSensitiveMemory, "metis run", &output); got != memory.ErrSensitiveMemory || output.Len() != 0 {
		t.Fatal("policy refusal was mislabeled a storage error")
	}
}

type failedRecallRepository struct {
	memory.Repository
	err error
}

func (r *failedRecallRepository) RecordTurn(context.Context, string, string, string, string) error {
	return r.err
}

func TestHeadlessRecallPolicyRejectionsRemainNonfatalButMixedFailuresDoNot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		fatal bool
	}{
		{"sensitive", memory.ErrSensitiveMemory, false},
		{"unsafe", memory.ErrUnsafeMemory, false},
		{"deleted", memory.ErrSessionDeleted, false},
		{"wrapped policy", fmt.Errorf("private-policy-context: %w", memory.ErrSensitiveMemory), false},
		{"joined policies", errors.Join(memory.ErrUnsafeMemory, memory.ErrSensitiveMemory), false},
		{"mixed policy and storage", errors.Join(memory.ErrSessionDeleted, &memory.RecallPersistenceError{Err: errors.New("private-storage-error")}), true},
		{"unclassified repository error", errors.New("private-repository-error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, err := memory.NewMemoryManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			loop := recallBoundaryLoop(&failedRecallRepository{Repository: manager, err: tc.err}, "recall-policy")
			loop.AppendUser("Remember the White Finch release codename.")
			var notices []string
			text, err := collectHeadlessEvents(func(events chan<- agent.Event) error {
				observed := make(chan agent.Event, 64)
				done := make(chan struct{})
				go func() {
					defer close(done)
					for event := range observed {
						if event.Kind == agent.EventInfo {
							notices = append(notices, event.Info)
						}
						events <- event
					}
				}()
				err := loop.Run(context.Background(), observed)
				close(observed)
				<-done
				return err
			})
			if (err != nil) != tc.fatal {
				t.Fatalf("error = %v, fatal = %v", err, tc.fatal)
			}
			if !strings.Contains(text, "White Finch") {
				t.Fatal("completed answer lost")
			}
			for _, notice := range notices {
				if strings.Contains(notice, "private-") {
					t.Fatal("recall notice leaked raw repository error")
				}
			}
		})
	}
}
