package webui

import (
	"context"
	"encoding/json"
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
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func addSelectionWorkspace(t *testing.T, h http.Handler, path string) workspaceView {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	rr := workspaceRequest(t, h, http.MethodPost, "/api/workspaces", string(body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("register workspace: %d %s", rr.Code, rr.Body.String())
	}
	var view workspaceView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func selectWorkspaceRequest(t *testing.T, h http.Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id})
	return workspaceRequest(t, h, http.MethodPost, "/api/workspaces/select", string(body))
}

func selectedWorkspaceID(t *testing.T, h http.Handler) string {
	t.Helper()
	rr := workspaceRequest(t, h, http.MethodGet, "/api/workspaces", "")
	var body struct {
		ActiveID string `json:"activeId"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &body) != nil {
		t.Fatalf("list selected workspace: %d %s", rr.Code, rr.Body.String())
	}
	return body.ActiveID
}

func TestWorkspaceSelectionStaysWindowLocalWithoutLaunchingOrRebinding(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	launch := t.TempDir()
	t.Chdir(launch)
	s, store := testServer(t)
	launched := false
	s.openWorkspace = func(string) error { launched = true; return nil }
	h := s.handler()
	other := t.TempDir()
	view := addSelectionWorkspace(t, h, other)
	rr := selectWorkspaceRequest(t, h, view.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("select workspace: %d %s", rr.Code, rr.Body.String())
	}
	var selected struct {
		ActiveID  string        `json:"activeId"`
		Workspace workspaceView `json:"workspace"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if selected.ActiveID != view.ID || selected.Workspace.Path != workspacePathKey(other) || !selected.Workspace.Active {
		t.Fatalf("select response = %+v", selected)
	}
	if launched || workspacePathKey(s.activeWorkDir) != workspacePathKey(launch) || workspacePathKey(s.freshWorkDir) != workspacePathKey(launch) {
		t.Fatalf("navigation mutated launch/runtime bindings: launched=%v active=%q fresh=%q", launched, s.activeWorkDir, s.freshWorkDir)
	}
	if cwd, _ := os.Getwd(); workspacePathKey(cwd) != workspacePathKey(launch) {
		t.Fatalf("selection changed process cwd: %q", cwd)
	}
	if got := selectedWorkspaceID(t, h); got != view.ID {
		t.Fatalf("active id = %q", got)
	}
	status := workspaceRequest(t, h, http.MethodGet, "/api/status", "")
	var statusBody struct {
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(status.Body.Bytes(), &statusBody) != nil || statusBody.Workspace != filepath.Base(other) {
		t.Fatalf("status workspace = %s", status.Body.String())
	}
	otherServer := NewServer("127.0.0.1:0", nil, store)
	if got := selectedWorkspaceID(t, otherServer.handler()); got != workspaceIDForPath(launch) {
		t.Fatalf("selection leaked to another window: %q", got)
	}
}

func TestWorkspaceSelectionRejectsMissingRemovedAndUnknownWithoutChangingSelection(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	h := s.handler()
	valid := addSelectionWorkspace(t, h, t.TempDir())
	if rr := selectWorkspaceRequest(t, h, valid.ID); rr.Code != http.StatusOK {
		t.Fatalf("select: %s", rr.Body.String())
	}
	removed := addSelectionWorkspace(t, h, t.TempDir())
	body, _ := json.Marshal(map[string]string{"id": removed.ID})
	workspaceRequest(t, h, http.MethodPost, "/api/workspaces/remove", string(body))
	missingDir := t.TempDir()
	missing := addSelectionWorkspace(t, h, missingDir)
	if err := os.Remove(missingDir); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{removed.ID, missing.ID, "unknown", ""} {
		rr := selectWorkspaceRequest(t, h, id)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			t.Fatalf("select invalid %q: %d %s", id, rr.Code, rr.Body.String())
		}
		if got := selectedWorkspaceID(t, h); got != valid.ID {
			t.Fatalf("failed selection changed active id: %q", got)
		}
	}
}

func TestNewSessionCapturesSelectedOrExplicitWorkspace(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, store := testServer(t)
	h := s.handler()
	a := addSelectionWorkspace(t, h, t.TempDir())
	b := addSelectionWorkspace(t, h, t.TempDir())
	if rr := selectWorkspaceRequest(t, h, a.ID); rr.Code != http.StatusOK {
		t.Fatalf("select: %s", rr.Body.String())
	}
	for _, test := range []struct{ body, want string }{{`{}`, a.Path}, {`{"workspaceId":"` + b.ID + `"}`, b.Path}} {
		rr := workspaceRequest(t, h, http.MethodPost, "/api/sessions", test.body)
		var item sessionItem
		if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &item) != nil {
			t.Fatalf("new session: %d %s", rr.Code, rr.Body.String())
		}
		hdr, _, err := store.LoadHeader(item.ID)
		if err != nil || hdr == nil || hdr.WorkDir != test.want || item.WorkDir != test.want || item.WorkspaceID != workspaceIDForPath(test.want) {
			t.Fatalf("new session workspace: item=%+v header=%+v err=%v", item, hdr, err)
		}
	}
	rr := workspaceRequest(t, h, http.MethodPost, "/api/sessions", `{"workspaceId":"unknown"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("invalid new session workspace: %d %s", rr.Code, rr.Body.String())
	}
	rr = workspaceRequest(t, h, http.MethodPost, "/api/sessions", `{"workspaceId":123}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed workspace id allocated a session: %d %s", rr.Code, rr.Body.String())
	}
}

// The ordinary tool path must receive the captured workspace, rather than
// merely rendering a different folder name in the sidebar.
type selectedWorkspaceReadTool struct {
	tools.BaseTool
	text string
}

func (*selectedWorkspaceReadTool) Name() string { return "CaptureCwd" }
func (*selectedWorkspaceReadTool) Description() string {
	return "read a relative fixture in the task working directory"
}
func (*selectedWorkspaceReadTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (*selectedWorkspaceReadTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*selectedWorkspaceReadTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t *selectedWorkspaceReadTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	data, err := os.ReadFile(filepath.Join(agent.CwdFromContext(ctx), "workspace-marker.txt"))
	t.text = string(data)
	return &tools.Result{Output: t.text}, err
}

func TestNewTurnUsesCapturedWorkspaceForRelativeToolRead(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	launch, target := t.TempDir(), t.TempDir()
	t.Chdir(launch)
	for dir, text := range map[string]string{launch: "launch marker", target: "selected marker"} {
		if err := os.WriteFile(filepath.Join(dir, "workspace-marker.txt"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &cwdCaptureProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
	read := &selectedWorkspaceReadTool{}
	registry := tools.NewRegistry()
	registry.Register(read)
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeAsk), nil, "system", 4)
	loop.Model = "model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire"})
	h := s.handler()
	targetView := addSelectionWorkspace(t, h, target)
	rr := workspaceRequest(t, h, http.MethodPost, "/api/turns", `{"input":"read marker","workspaceId":"`+targetView.ID+`"}`)
	if rr.Code != http.StatusOK || read.text != "selected marker" {
		t.Fatalf("relative read: code=%d text=%q body=%s", rr.Code, read.text, rr.Body.String())
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &result)
	hdr, _, err := store.LoadHeader(result.SessionID)
	if err != nil || hdr.WorkDir != workspacePathKey(target) {
		t.Fatalf("turn header=%+v err=%v", hdr, err)
	}
}

func TestWorkspaceSelectionDuringWorkerDoesNotMoveOrCancelExistingTurn(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 2), release: release}
	s := NewServer("127.0.0.1:0", agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2), store, RuntimeBindings{IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{MaxParallel: 2}})
	h := s.handler()
	a := addSelectionWorkspace(t, h, t.TempDir())
	b := addSelectionWorkspace(t, h, t.TempDir())
	if err := store.WriteHeaderFull(session.Header{ID: "worker-a", WorkDir: a.Path, Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- workspaceRequest(t, h, http.MethodPost, "/api/turns", `{"sessionId":"worker-a","workspaceId":"`+b.ID+`","input":"A"}`)
	}()
	var first IsolatedTurnRequest
	select {
	case first = <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("worker A did not start")
	}
	if first.WorkDir != a.Path {
		t.Fatalf("client moved existing session: %q", first.WorkDir)
	}
	if rr := selectWorkspaceRequest(t, h, b.ID); rr.Code != http.StatusOK {
		t.Fatalf("select while A runs: %d %s", rr.Code, rr.Body.String())
	}
	go func() { responses <- workspaceRequest(t, h, http.MethodPost, "/api/turns", `{"input":"B"}`) }()
	select {
	case second := <-runner.started:
		if second.WorkDir != b.Path {
			t.Fatalf("new worker B cwd=%q", second.WorkDir)
		}
	case <-time.After(time.Second):
		t.Fatal("worker B did not start while A runs")
	}
	if first.WorkDir != a.Path || selectedWorkspaceID(t, h) != b.ID {
		t.Fatal("navigation changed worker A ownership or B selection")
	}
	// Finish both workers and verify completion does not restore A's navigation.
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		select {
		case rr := <-responses:
			if rr.Code != http.StatusOK {
				t.Fatalf("worker response: %d %s", rr.Code, rr.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("workers did not finish")
		}
	}
	if selectedWorkspaceID(t, h) != b.ID {
		t.Fatal("worker completion overwrote selected workspace")
	}
}

func TestWorkspaceExplicitActivationSelectsHeaderButLegacyTurnKeepsLaunchFallback(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	launch, other := t.TempDir(), t.TempDir()
	t.Chdir(launch)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &statusTestProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire"})
	h := s.handler()
	otherView := addSelectionWorkspace(t, h, other)
	for _, hdr := range []session.Header{{ID: "selected-history", WorkDir: other, Provider: "wire", Model: "model"}, {ID: "legacy-history", Provider: "wire", Model: "model"}} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	rr := workspaceRequest(t, h, http.MethodPost, "/api/sessions/activate", `{"id":"selected-history"}`)
	if rr.Code != http.StatusOK || selectedWorkspaceID(t, h) != otherView.ID {
		t.Fatalf("explicit activation did not select header: %d %s", rr.Code, rr.Body.String())
	}
	rr = workspaceRequest(t, h, http.MethodPost, "/api/turns", `{"sessionId":"legacy-history","input":"legacy"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy turn: %d %s", rr.Code, rr.Body.String())
	}
	if workspacePathKey(s.activeWorkDir) != workspacePathKey(launch) || workspacePathKey(s.freshWorkDir) != workspacePathKey(launch) || selectedWorkspaceID(t, h) != otherView.ID {
		t.Fatalf("legacy fallback or selection changed: runtime=%q fresh=%q selected=%q", s.activeWorkDir, s.freshWorkDir, selectedWorkspaceID(t, h))
	}
	if strings.TrimSpace(s.activeWorkDir) == "" {
		t.Fatal("legacy workspace empty")
	}
}

func TestQueuedFreshTurnKeepsCapturedWorkspaceAcrossNavigation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	close(release)
	runner := &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 1), release: release}
	s := NewServer("127.0.0.1:0", agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2), store, RuntimeBindings{IsolatedRunner: runner, IsolatedTurns: &IsolatedTurnOptions{MaxParallel: 1}})
	h := s.handler()
	a := addSelectionWorkspace(t, h, t.TempDir())
	b := addSelectionWorkspace(t, h, t.TempDir())
	if rr := selectWorkspaceRequest(t, h, a.ID); rr.Code != http.StatusOK {
		t.Fatalf("select A: %s", rr.Body.String())
	}
	held, ok := s.turnCoordinator.TryAcquire("held-global-slot")
	if !ok {
		t.Fatal("reserve slot")
	}
	defer held.Release()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- workspaceRequest(t, h, http.MethodPost, "/api/turns", `{"input":"captured A"}`) }()
	deadline := time.After(time.Second)
	for {
		s.turnCoordinator.mu.Lock()
		waiting := len(s.turnCoordinator.waiters) > 0
		s.turnCoordinator.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("fresh request did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	if rr := selectWorkspaceRequest(t, h, b.ID); rr.Code != http.StatusOK {
		t.Fatalf("navigation blocked by full slots: %d %s", rr.Code, rr.Body.String())
	}
	held.Release()
	select {
	case req := <-runner.started:
		if req.WorkDir != a.Path {
			t.Fatalf("queued A moved to %q", req.WorkDir)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not start")
	}
	select {
	case rr := <-done:
		if rr.Code != http.StatusOK {
			t.Fatalf("queued response: %d %s", rr.Code, rr.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not complete")
	}
	if selectedWorkspaceID(t, h) != b.ID {
		t.Fatal("queued completion overwrote B navigation")
	}
}

func TestSlowActivationDoesNotOverwriteNewerWorkspaceSelection(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := t.TempDir(), t.TempDir()
	if err := store.WriteHeaderFull(session.Header{ID: "slow-activate", WorkDir: a, Provider: "wire", Model: "model"}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	provider := &statusTestProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire", PrepareSessionSwitch: func(id, dir string) (string, func(), error) {
		if id == "slow-activate" {
			close(started)
			<-release
		}
		return workspacePathKey(dir), func() {}, nil
	}})
	h := s.handler()
	viewB := addSelectionWorkspace(t, h, b)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- workspaceRequest(t, h, http.MethodPost, "/api/sessions/activate", `{"id":"slow-activate"}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("activation did not reach preflight")
	}
	if rr := selectWorkspaceRequest(t, h, viewB.ID); rr.Code != http.StatusOK {
		t.Fatalf("select B: %d %s", rr.Code, rr.Body.String())
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case rr := <-done:
		if rr.Code != http.StatusOK {
			t.Fatalf("activate A: %d %s", rr.Code, rr.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("activation did not finish")
	}
	if selectedWorkspaceID(t, h) != viewB.ID || workspacePathKey(s.activeWorkDir) != workspacePathKey(a) {
		t.Fatalf("late activation moved selection or failed runtime binding: runtime=%q", s.activeWorkDir)
	}
}

func TestCancelledWorkspaceSelectionKeepsPriorSelection(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	h := s.handler()
	before := selectedWorkspaceID(t, h)
	view := addSelectionWorkspace(t, h, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/select", strings.NewReader(`{"id":"`+view.ID+`"}`)).WithContext(ctx)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestTimeout || selectedWorkspaceID(t, h) != before {
		t.Fatalf("cancelled selection committed: %d %s", rr.Code, rr.Body.String())
	}
}

func TestLegacySessionResponsesExposeStableEffectiveWorkspaceWithoutRewritingHeader(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	launch := t.TempDir()
	t.Chdir(launch)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{ID: "legacy-response", Provider: "wire", Model: "model"}); err != nil {
		t.Fatal(err)
	}
	provider := &statusTestProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire"})
	h := s.handler()
	other := addSelectionWorkspace(t, h, t.TempDir())
	if rr := selectWorkspaceRequest(t, h, other.ID); rr.Code != http.StatusOK {
		t.Fatalf("select other: %s", rr.Body.String())
	}
	for _, request := range []struct{ method, path, body string }{{http.MethodGet, "/api/sessions/legacy-response", ""}, {http.MethodPost, "/api/sessions/activate", `{"id":"legacy-response"}`}} {
		rr := workspaceRequest(t, h, request.method, request.path, request.body)
		var response struct {
			Session   sessionItem   `json:"session"`
			Workspace workspaceView `json:"workspace"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &response) != nil {
			t.Fatalf("legacy metadata response: %d %s", rr.Code, rr.Body.String())
		}
		wantPath, wantID := workspacePathKey(launch), workspaceIDForPath(launch)
		if response.Session.WorkDir != wantPath || response.Session.WorkspaceID != wantID || response.Workspace.Path != wantPath || response.Workspace.ID != wantID || !response.Workspace.Available || response.Workspace.Name != filepath.Base(launch) {
			t.Fatalf("legacy response omitted effective launch binding: %+v", response)
		}
		hdr, _, err := store.LoadHeader("legacy-response")
		if err != nil || hdr.WorkDir != "" {
			t.Fatalf("legacy metadata rewrote stored header: %+v err=%v", hdr, err)
		}
	}
	if selectedWorkspaceID(t, h) != workspaceIDForPath(launch) {
		t.Fatal("explicit legacy activation did not select effective launch directory")
	}
}

func TestReadOnlySessionResponseKeepsHistoryWhenWorkspaceUnavailable(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, store := testServer(t)
	h := s.handler()
	path := filepath.Join(t.TempDir(), "removed-workspace")
	if err := store.WriteHeaderFull(session.Header{ID: "missing-history", WorkDir: path}); err != nil {
		t.Fatal(err)
	}
	message := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "saved answer"}}}
	if err := store.AppendMessage("missing-history", message); err != nil {
		t.Fatal(err)
	}
	rr := workspaceRequest(t, h, http.MethodGet, "/api/sessions/missing-history", "")
	var response struct {
		Session   sessionItem   `json:"session"`
		Workspace workspaceView `json:"workspace"`
		Messages  []llm.Message `json:"messages"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &response) != nil {
		t.Fatalf("read-only missing directory: %d %s", rr.Code, rr.Body.String())
	}
	if response.Workspace.Available || response.Workspace.ID != workspaceIDForPath(path) || response.Session.WorkDir != workspacePathKey(path) || len(response.Messages) != 1 {
		t.Fatalf("missing directory hid history or lost owner: %+v", response)
	}
	if rr := selectWorkspaceRequest(t, h, response.Workspace.ID); rr.Code != http.StatusBadRequest {
		t.Fatalf("unavailable owner became selectable: %d %s", rr.Code, rr.Body.String())
	}
}
