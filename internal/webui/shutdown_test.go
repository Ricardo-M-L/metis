package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const testDesktopShutdownToken = "native-desktop-secret"

type countingMemoryRepository struct {
	memory.Repository
	mu      sync.Mutex
	sources []string
	saveErr error
	onSave  func(sessionID, source string) error
}

func (repo *countingMemoryRepository) SaveDailyNote(sessionID, source, summary string) error {
	repo.mu.Lock()
	repo.sources = append(repo.sources, source)
	err := repo.saveErr
	onSave := repo.onSave
	repo.mu.Unlock()
	if err != nil {
		return err
	}
	if onSave != nil {
		if err := onSave(sessionID, source); err != nil {
			return err
		}
	}
	return repo.Repository.SaveDailyNote(sessionID, source, summary)
}

func (repo *countingMemoryRepository) savedSources() []string {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	return append([]string(nil), repo.sources...)
}

func TestPersistActiveSessionBoundarySkipsDailyWhenHeaderFails(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &countingMemoryRepository{Repository: manager}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = repository
	loop.Restore([]llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "must not become a false daily boundary"}},
	}})
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "header-failure",
		ProviderName:     "test",
	})

	notDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Dir = notDirectory

	err = server.persistActiveSessionBoundary("desktop-close")
	if err == nil {
		t.Fatal("boundary succeeded despite an unwritable session header")
	}
	if sources := repository.savedSources(); len(sources) != 0 {
		t.Fatalf("Daily was written before the header durability boundary: %v", sources)
	}
}

func TestPersistActiveSessionBoundaryReturnsDailyFailure(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	dailyErr := errors.New("daily persistence failed")
	repository := &countingMemoryRepository{Repository: manager, saveErr: dailyErr}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = repository
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "daily-failure",
		ProviderName:     "test",
	})

	err = server.persistActiveSessionBoundary("desktop-close")
	if !errors.Is(err, dailyErr) {
		t.Fatalf("boundary error = %v, want Daily persistence error", err)
	}
	if _, _, loadErr := store.Load("daily-failure"); loadErr != nil {
		t.Fatalf("session header was not persisted before Daily: %v", loadErr)
	}
	if sources := repository.savedSources(); len(sources) != 1 || sources[0] != "desktop-close" {
		t.Fatalf("Daily attempts = %v, want one desktop-close attempt", sources)
	}
}

func TestDesktopClosePersistsBoundaryAfterBackgroundMemoryWaitFailures(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &countingMemoryRepository{Repository: manager}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = repository
	loop.Restore([]llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "preserve this legal close boundary"}},
	}})
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "background-wait-failure",
		ProviderName:     "test",
	})
	distillErr := errors.New("distillation wait failed")
	autoMemoryErr := errors.New("auto memory wait failed")
	var flushedSession string
	server.flushPendingDistillation = func(sessionID string) int {
		flushedSession = sessionID
		return 1
	}
	server.waitForDistillation = func(context.Context, string) error { return distillErr }
	server.waitAutoMemoryIdle = func(context.Context) error { return autoMemoryErr }

	err = server.persistDesktopCloseWithTimeouts(desktopCloseTimeouts{
		turn:         20 * time.Millisecond,
		distillation: 20 * time.Millisecond,
		autoMemory:   20 * time.Millisecond,
	})
	if !errors.Is(err, distillErr) || !errors.Is(err, autoMemoryErr) {
		t.Fatalf("close error = %v, want both background wait errors", err)
	}
	if flushedSession != "" {
		t.Fatalf("Desktop close flushed session %q, want all pending sessions", flushedSession)
	}
	if sources := repository.savedSources(); len(sources) != 1 || sources[0] != "desktop-close" {
		t.Fatalf("valid close boundary was skipped after background errors: %v", sources)
	}
	if _, _, loadErr := store.Load("background-wait-failure"); loadErr != nil {
		t.Fatalf("session header was skipped after background errors: %v", loadErr)
	}
}

func TestDesktopCloseSkipsDailyWhileForegroundTurnCanStillMutateHistory(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &countingMemoryRepository{Repository: manager}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = repository
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "foreground-still-running",
		ProviderName:     "test",
	})
	server.turnDone = make(chan struct{})
	server.cancelTurn = func() {}

	err = server.persistDesktopCloseWithTimeouts(desktopCloseTimeouts{
		turn:         10 * time.Millisecond,
		distillation: 10 * time.Millisecond,
		autoMemory:   10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("close succeeded while foreground history could still change")
	}
	if sources := repository.savedSources(); len(sources) != 0 {
		t.Fatalf("Daily captured a moving foreground history: %v", sources)
	}
}

func TestDesktopSessionSwitchFlushesResidualBeforeDaily(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const currentID = "residual-current"
	const nextID = "residual-next"
	for _, id := range []string{currentID, nextID} {
		if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: "model", System: "system"}); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	flushed := false
	waited := false
	repository := &countingMemoryRepository{Repository: manager}
	repository.onSave = func(sessionID, source string) error {
		if !waited {
			return errors.New("Daily ran before residual distillation was joined")
		}
		if sessionID != currentID || source != "desktop-switch" {
			return fmt.Errorf("Daily boundary = %s/%s", sessionID, source)
		}
		return nil
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = repository
	loop.Restore([]llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "short session fact"}},
	}})
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: currentID,
		ProviderName:     "wire",
	})
	server.flushPendingDistillation = func(sessionID string) int {
		if sessionID != currentID {
			t.Fatalf("flushed session = %q, want %q", sessionID, currentID)
		}
		flushed = true
		return 1
	}
	server.waitForDistillation = func(_ context.Context, sessionID string) error {
		if !flushed {
			return errors.New("wait ran before residual registration")
		}
		if sessionID != currentID {
			return fmt.Errorf("waited session = %q", sessionID)
		}
		waited = true
		return nil
	}

	err = server.activateSession(nextID, &session.Header{
		ID: nextID, Provider: "wire", Model: "model", System: "system",
	}, nil)
	if err != nil {
		t.Fatalf("activate next session: %v", err)
	}
	if !flushed || !waited {
		t.Fatalf("residual lifecycle flushed=%t waited=%t", flushed, waited)
	}
	if sources := repository.savedSources(); len(sources) != 1 || sources[0] != "desktop-switch" {
		t.Fatalf("Daily saves = %v, want one post-flush desktop-switch", sources)
	}
}

func TestDesktopSessionSwitchWaitFailureKeepsCurrentSessionAndSkipsDaily(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const currentID = "wait-failure-current"
	const nextID = "wait-failure-next"
	for _, id := range []string{currentID, nextID} {
		if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: "model", System: "system"}); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &countingMemoryRepository{Repository: manager}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = repository
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: currentID,
		ProviderName:     "wire",
	})
	waitErr := errors.New("residual wait failed")
	server.flushPendingDistillation = func(string) int { return 1 }
	server.waitForDistillation = func(context.Context, string) error { return waitErr }

	err = server.activateSession(nextID, &session.Header{
		ID: nextID, Provider: "wire", Model: "model", System: "system",
	}, nil)
	if !errors.Is(err, waitErr) {
		t.Fatalf("activate error = %v, want residual wait failure", err)
	}
	server.stateMu.RLock()
	activeID := server.activeSessionID
	server.stateMu.RUnlock()
	if activeID != currentID {
		t.Fatalf("active session = %q, want current %q after failed boundary", activeID, currentID)
	}
	if sources := repository.savedSources(); len(sources) != 0 {
		t.Fatalf("Daily was written for a failed switch: %v", sources)
	}
}

func TestDesktopShutdownEndpointIsUnavailableWithoutNativeToken(t *testing.T) {
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		Shutdown: func() { t.Error("browser-mode shutdown callback must not run") },
	})

	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d when native shutdown is disabled", response.Code, http.StatusNotFound)
	}
}

func TestDesktopShutdownEndpointRequiresNativeToken(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown:      func() { called <- struct{}{} },
	})

	for _, token := range []string{"", "wrong-token"} {
		request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
		request.Host = "127.0.0.1:8080"
		request.Header.Set(desktopShutdownTokenHeader, token)
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("token %q status = %d, want %d", token, response.Code, http.StatusForbidden)
		}
	}

	select {
	case <-called:
		t.Fatal("shutdown callback ran for a missing or incorrect token")
	default:
	}
}

func TestDesktopShutdownEndpointCancelsServerWithNativeToken(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown:      func() { called <- struct{}{} },
	})
	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()

	server.handler().ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("authorized shutdown did not cancel the server")
	}
}

func TestDesktopShutdownPersistsDailyMemory(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "desktop-shutdown-memory"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "test", Model: "model", System: "system"}); err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	countingMemory := &countingMemoryRepository{Repository: manager}
	loop.Memory = countingMemory
	loop.Restore([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "persist this shutdown fact"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "shutdown fact acknowledged"}}},
	})
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: id,
		ProviderName:     "test",
		ShutdownToken:    testDesktopShutdownToken,
		Shutdown:         func() { called <- struct{}{} },
	})
	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not reached")
	}
	// The endpoint's Shutdown callback normally cancels Server.Run, which
	// reaches the same close hook a second time. Simulate that path and assert
	// the lifecycle guard keeps the final daily write idempotent.
	server.persistDesktopClose()
	notes, err := manager.ListDailyNotes(10)
	if err != nil || len(notes) != 1 || notes[0].SessionID != id ||
		!containsAll(notes[0].Summary, "shutdown fact", "acknowledged") {
		t.Fatalf("shutdown daily memory missing: notes=%+v err=%v", notes, err)
	}
	if notes[0].Source != "desktop-close" {
		t.Fatalf("shutdown daily source = %q, want desktop-close", notes[0].Source)
	}
	if sources := countingMemory.savedSources(); len(sources) != 1 || sources[0] != "desktop-close" {
		t.Fatalf("shutdown daily saves = %v, want exactly one desktop-close", sources)
	}
}

func TestServerRunContextCancellationPersistsDesktopCloseDailyOnce(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "desktop-context-cancel-memory"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "test", Model: "model", System: "system"}); err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	countingMemory := &countingMemoryRepository{Repository: manager}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Memory = countingMemory
	loop.Restore([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "context cancellation fact"}}},
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(addr, loop, store, RuntimeBindings{
		InitialSessionID: id,
		ProviderName:     "test",
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	waitForDesktopHealth(t, addr)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after context cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}

	if sources := countingMemory.savedSources(); len(sources) != 1 || sources[0] != "desktop-close" {
		t.Fatalf("context cancellation daily saves = %v, want exactly one desktop-close", sources)
	}
	notes, err := manager.ListDailyNotes(10)
	if err != nil || len(notes) != 1 || notes[0].Source != "desktop-close" ||
		!strings.Contains(notes[0].Summary, "context cancellation fact") {
		t.Fatalf("context cancellation daily memory missing: notes=%+v err=%v", notes, err)
	}
}

func TestRunHTTPServerJoinsActiveHandlersBeforeReturning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/hold", func(w http.ResponseWriter, _ *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runHTTPServer(ctx, server, time.Second, nil) }()
	waitForDesktopHealthPath(t, addr, "/health")

	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + addr + "/hold")
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("held handler did not start")
	}
	cancel()
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before its active handler drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("held request failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("held request did not finish after release")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run after graceful drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after active handler drained")
	}
}

func waitForDesktopHealthPath(t *testing.T, addr, path string) {
	t.Helper()
	client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + addr + path)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("health endpoint at %s%s did not become ready", addr, path)
}

func waitForDesktopHealth(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{
		Timeout:   250 * time.Millisecond,
		Transport: &http.Transport{Proxy: nil},
	}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + addr + "/api/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Desktop health endpoint at %s did not become ready", addr)
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func TestDesktopShutdownEndpointRejectsNonLoopbackHost(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown:      func() { called <- struct{}{} },
	})
	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "example.com"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()

	server.handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d for a non-loopback Host", response.Code, http.StatusForbidden)
	}
	select {
	case <-called:
		t.Fatal("shutdown callback ran for a non-loopback request")
	default:
	}
}

func TestDesktopShutdownCancelsActiveTurnBeforeServer(t *testing.T) {
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	defer cancelTurn()
	turnDone := make(chan struct{})
	go func() {
		<-turnCtx.Done()
		close(turnDone)
	}()
	permissionReply := make(chan agent.PermissionDecision, 1)
	askReply := make(chan string, 1)
	shutdownCalled := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown: func() {
			select {
			case <-turnDone:
				shutdownCalled <- struct{}{}
			default:
				t.Error("server shutdown ran before the active turn stopped")
			}
		},
	})
	server.cancelTurn = cancelTurn
	server.turnDone = turnDone
	server.pendingPerms["permission"] = &permissionPending{reply: permissionReply}
	server.pendingAsks["ask"] = &askPending{reply: askReply}

	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)

	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("server shutdown did not wait for active turn cancellation")
	}
	select {
	case decision := <-permissionReply:
		if decision != agent.PermissionDecisionDeny {
			t.Fatalf("permission reply = %v, want deny", decision)
		}
	default:
		t.Fatal("pending permission was not released")
	}
	select {
	case answer := <-askReply:
		if answer != "" {
			t.Fatalf("ask reply = %q, want empty cancellation", answer)
		}
	default:
		t.Fatal("pending AskUser interaction was not released")
	}
}

func TestTurnWaitRemainsBoundedAndFailClosed(t *testing.T) {
	done := make(chan struct{})
	started := time.Now()
	if waitForTurnShutdown(done, 20*time.Millisecond) {
		t.Fatal("close unexpectedly reported a stopped turn")
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed > time.Second {
		t.Fatalf("turn fallback elapsed=%s, want a bounded wait near 20ms", elapsed)
	}
}

func TestQueuedTurnCannotStartAfterDesktopShutdownBegins(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{})

	server.runMu.Lock()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"input":"must not start"}`))
		server.handler().ServeHTTP(response, request)
		close(done)
	}()
	// The request may be decoding or queued on runMu; either state is safe as
	// long as the post-lock closing check is authoritative.
	time.Sleep(20 * time.Millisecond)
	server.beginClosing()
	server.runMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued turn did not return after shutdown began")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("queued turn status=%d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	entries, err := store.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("queued turn created a ghost session during shutdown: %+v", entries)
	}
}
