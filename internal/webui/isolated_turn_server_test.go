package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type blockingIsolatedRunner struct {
	started chan IsolatedTurnRequest
	release <-chan struct{}
}

func (r *blockingIsolatedRunner) Eligible(*session.Header) bool { return true }

func (r *blockingIsolatedRunner) Run(ctx context.Context, request IsolatedTurnRequest) (IsolatedTurnResult, error) {
	if request.OnText != nil {
		request.OnText("live: " + request.Input)
	}
	select {
	case r.started <- request:
	case <-ctx.Done():
		return IsolatedTurnResult{Stopped: true}, ctx.Err()
	}
	select {
	case <-r.release:
		return IsolatedTurnResult{Text: "complete: " + request.Input}, nil
	case <-ctx.Done():
		return IsolatedTurnResult{Stopped: true}, ctx.Err()
	}
}

func TestDesktopIsolatedTurnsStartInParallelAcrossWorkspaces(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceA, workspaceB := t.TempDir(), t.TempDir()
	for _, item := range []struct{ id, workdir string }{{"parallel-a", workspaceA}, {"parallel-b", workspaceB}} {
		if err := store.WriteHeaderFull(session.Header{ID: item.id, WorkDir: item.workdir, Mode: string(permission.ModeFullAccess), Status: "idle"}); err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 2), release: release}
	server := NewServer("127.0.0.1:0", agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2), store, RuntimeBindings{
		IsolatedRunner: runner,
		IsolatedTurns:  &IsolatedTurnOptions{Executable: "/ignored-in-test", MaxParallel: 2},
	})

	responses := make(chan *httptest.ResponseRecorder, 2)
	var group sync.WaitGroup
	for _, item := range []struct{ id, input string }{{"parallel-a", "one"}, {"parallel-b", "two"}} {
		group.Add(1)
		go func(id, input string) {
			defer group.Done()
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"sessionId":"`+id+`","input":"`+input+`"}`))
			server.handler().ServeHTTP(response, request)
			responses <- response
		}(item.id, item.input)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("independent workspace turn did not start in parallel")
		}
	}
	close(release)
	group.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("isolated turn status = %d, body=%s", response.Code, response.Body.String())
		}
	}
}

func TestProcessIsolatedTurnRunnerEligibility(t *testing.T) {
	runner := &processIsolatedTurnRunner{executable: "/metis"}
	for _, mode := range []permission.Mode{permission.ModeDontAsk, permission.ModeBypassPermissions, permission.ModeFullAccess} {
		if !runner.Eligible(&session.Header{Mode: string(mode)}) {
			t.Fatalf("mode %q should use an isolated worker", mode)
		}
	}
	for _, mode := range []permission.Mode{permission.ModeAsk, permission.ModeAcceptEdits, permission.ModePlan} {
		if runner.Eligible(&session.Header{Mode: string(mode)}) {
			t.Fatalf("approval-driven mode %q should remain in-process", mode)
		}
	}
	if runner.Eligible(nil) {
		t.Fatal("nil header should not be eligible")
	}
}

func TestProcessIsolatedTurnRunnerStreamsChildOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell")
	}
	workdir := t.TempDir()
	script := filepath.Join(workdir, "metis-fixture")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'first ' \nprintf 'second'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := newProcessIsolatedTurnRunner(IsolatedTurnOptions{Executable: script})
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	result, err := runner.Run(context.Background(), IsolatedTurnRequest{
		SessionID: "worker-fixture",
		WorkDir:   workdir,
		Input:     "hello",
		OnText: func(delta string) {
			deltas.WriteString(delta)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "first second" {
		t.Fatalf("worker text = %q", result.Text)
	}
	if deltas.String() != result.Text {
		t.Fatalf("streamed text = %q, want %q", deltas.String(), result.Text)
	}
}

func TestIsolatedWorkerEnvironmentOverridesCriticalPaths(t *testing.T) {
	env := withIsolatedWorkerEnv([]string{
		"KEEP=value",
		"METIS_AUTO_MEMORY=1",
		"METIS_DESKTOP_SUBAGENT_SLOTS=99",
	}, map[string]string{
		"METIS_AUTO_MEMORY":               "0",
		"METIS_DESKTOP_SUBAGENT_SLOTS":    "6",
		"METIS_DESKTOP_SUBAGENT_CAP":      "4",
		"METIS_DESKTOP_SUBAGENT_SLOT_DIR": "/tmp/slots",
	})
	got := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			got[key] = value
		}
	}
	if got["KEEP"] != "value" {
		t.Fatalf("unrelated environment lost: %q", got["KEEP"])
	}
	for key, want := range map[string]string{
		"METIS_AUTO_MEMORY": "0", "METIS_DESKTOP_SUBAGENT_SLOTS": "6",
		"METIS_DESKTOP_SUBAGENT_CAP": "4", "METIS_DESKTOP_SUBAGENT_SLOT_DIR": "/tmp/slots",
	} {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestLatestAssistantText(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "old"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "next"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "new"}, {Type: "text", Text: " reply"}}},
	}
	if got := latestAssistantText(history); got != "new reply" {
		t.Fatalf("latestAssistantText = %q", got)
	}
}
