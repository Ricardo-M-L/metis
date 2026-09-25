package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func seedComposerHistory(t *testing.T, store *session.Store, id, workspace string) []llm.Message {
	t.Helper()
	if err := store.WriteHeaderFull(session.Header{
		ID: id, WorkDir: workspace, Mode: string(permission.ModeDefault), Provider: "wire", Model: "composer-summary", System: "system",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		if err := store.AppendMessage(id, llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("history ", 200)}}}); err != nil {
			t.Fatal(err)
		}
	}
	_, history, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

func TestComposerHistoryMutationsRejectRunningIsolatedWorkspace(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	before := map[string][]llm.Message{
		"editing-active":  seedComposerHistory(t, store, "editing-active", workspace),
		"editing-sibling": seedComposerHistory(t, store, "editing-sibling", workspace),
	}
	seedComposerHistory(t, store, "editing-unrelated", t.TempDir())
	provider := &composerSummaryProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2)
	loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), provider.ModelID(), provider.MaxContextTokens(), provider)
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 1), release: make(chan struct{})}
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		ProviderName: "wire", IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{Executable: "/ignored-in-test", MaxParallel: 2},
	})
	ctx, cancel := context.WithCancel(context.Background())
	turnDone := make(chan *httptest.ResponseRecorder, 1)
	t.Cleanup(func() { cancel(); <-turnDone })
	go func() {
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"sessionId":"editing-active","input":"work"}`)).WithContext(ctx))
		turnDone <- response
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("isolated turn did not start")
	}
	for id, history := range before {
		for _, command := range []string{"undo", "retry", "clear-history", "save", "compact"} {
			t.Run(id+"/"+command, func(t *testing.T) {
				path := "/api/commands/session"
				if command == "compact" {
					path = "/api/compact"
				}
				response := httptest.NewRecorder()
				body := fmt.Sprintf(`{"sessionId":%q,"command":%q}`, id, command)
				server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
				if response.Code != http.StatusConflict {
					t.Fatalf("busy mutation status = %d: %s", response.Code, response.Body.String())
				}
				_, after, err := store.Load(id)
				if err != nil || !reflect.DeepEqual(after, history) {
					t.Fatalf("busy command changed durable history: err=%v", err)
				}
			})
		}
	}
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/commands/session", strings.NewReader(`{"sessionId":"editing-unrelated","command":"undo"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("unrelated idle workspace mutation was blocked: %d %s", response.Code, response.Body.String())
	}
}

type blockingComposerSummaryProvider struct {
	composerSummaryProvider
	started chan struct{}
	release <-chan struct{}
}

func (provider *blockingComposerSummaryProvider) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	close(provider.started)
	select {
	case <-provider.release:
		return provider.composerSummaryProvider.Stream(ctx, request)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestManualCompactionHoldsWorkspaceUntilDurableReplacement(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	before := seedComposerHistory(t, store, "compact-before-turn", workspace)
	providerRelease := make(chan struct{})
	provider := &blockingComposerSummaryProvider{started: make(chan struct{}), release: providerRelease}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2)
	compactConfig := agent.DefaultCompactionConfig()
	compactConfig.ProtectFirst, compactConfig.ProtectLast = 1, 1
	loop.Compactor = agent.NewCompactor(compactConfig, provider.ModelID(), provider.MaxContextTokens(), provider)
	workerRelease := make(chan struct{})
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 1), release: workerRelease}
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		ProviderName: "wire", IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{Executable: "/ignored-in-test", MaxParallel: 2},
	})
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	t.Cleanup(func() { cancel(); group.Wait() })
	compactDone := make(chan *httptest.ResponseRecorder, 1)
	group.Add(1)
	go func() {
		defer group.Done()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/compact", strings.NewReader(`{"sessionId":"compact-before-turn"}`)).WithContext(ctx)
		server.handler().ServeHTTP(response, request)
		compactDone <- response
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("compaction provider did not start")
	}
	turnDone := make(chan *httptest.ResponseRecorder, 1)
	group.Add(1)
	go func() {
		defer group.Done()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"sessionId":"compact-before-turn","input":"run after compact"}`)).WithContext(ctx)
		server.handler().ServeHTTP(response, request)
		turnDone <- response
	}()
	deadline := time.Now().Add(time.Second)
	queued := false
	for time.Now().Before(deadline) {
		server.turnCoordinator.mu.Lock()
		queued = len(server.turnCoordinator.waiters) == 1
		server.turnCoordinator.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-runner.started:
			t.Fatal("worker started before manual compaction persisted its replacement")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if !queued {
		t.Fatal("same-workspace turn did not wait for the compaction lease")
	}
	close(providerRelease)
	select {
	case result := <-compactDone:
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"compacted":true`) {
			t.Fatalf("compaction status = %d: %s", result.Code, result.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("compaction did not complete")
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("worker was not admitted after compaction released its lease")
	}
	_, durable, err := store.Load("compact-before-turn")
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(durable, before) {
		t.Fatal("worker was admitted without compacted durable history")
	}
	close(workerRelease)
	select {
	case result := <-turnDone:
		if result.Code != http.StatusOK {
			t.Fatalf("worker status = %d: %s", result.Code, result.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not finish")
	}
}
