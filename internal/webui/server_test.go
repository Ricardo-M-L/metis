package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type activationTestProvider struct {
	name, model string
}

func (p *activationTestProvider) Name() string          { return p.name }
func (p *activationTestProvider) ModelID() string       { return p.model }
func (p *activationTestProvider) MaxContextTokens() int { return 128_000 }
func (p *activationTestProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{StopReason: "end_turn"}, nil
}
func (p *activationTestProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("stream not used by activation tests")
}

type statusTestStream struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
	done    bool
}

func (s *statusTestStream) Recv() (llm.StreamEvent, error) {
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
		<-s.ctx.Done()
		return llm.StreamEvent{}, s.ctx.Err()
	}
	if s.done {
		return llm.StreamEvent{}, io.EOF
	}
	s.done = true
	return llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
}

func (s *statusTestStream) Close() error { return nil }

type statusTestProvider struct {
	activationTestProvider
	started chan struct{}
}

func (p *statusTestProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	return &statusTestStream{ctx: ctx, started: p.started}, nil
}

type autoCompactPersistenceProvider struct{}

func (*autoCompactPersistenceProvider) Name() string    { return "auto-compact-persistence" }
func (*autoCompactPersistenceProvider) ModelID() string { return "auto-compact-persistence" }

// Keep the window small enough to force this fixture's 1% auto-compaction
// threshold, but large enough to hold the real structured summary prompt.
// A synthetic 128-token window is now correctly rejected by the wire-budget
// guard and therefore cannot exercise persistence semantics.
func (*autoCompactPersistenceProvider) MaxContextTokens() int { return 8_000 }
func (*autoCompactPersistenceProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("test provider expects streaming")
}
func (*autoCompactPersistenceProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	text := "final answer"
	if strings.Contains(req.System, "summarizing an agent conversation") {
		text = "AUTO_COMPACT_SUMMARY"
	}
	return &composerSummaryStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: text},
		{Type: "message_stop", StopReason: "end_turn"},
	}}, nil
}

type turnTailFailureProvider struct {
	mu            sync.Mutex
	calls         int
	blockOnSecond bool
	secondStarted chan struct{}
}

func (*turnTailFailureProvider) Name() string          { return "turn-tail-failure" }
func (*turnTailFailureProvider) ModelID() string       { return "turn-tail-failure" }
func (*turnTailFailureProvider) MaxContextTokens() int { return 128_000 }
func (*turnTailFailureProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("test provider expects streaming")
}
func (p *turnTailFailureProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		return &composerSummaryStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "persist-1", ToolName: "PersistMarker"},
			{Type: "tool_input_delta", ToolUseID: "persist-1", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "persist-1", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	if p.blockOnSecond {
		if call == 2 && p.secondStarted != nil {
			close(p.secondStarted)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("forced follow-up failure")
}

type turnTailMarkerTool struct {
	tools.BaseTool
	poisonPath string
}

func (turnTailMarkerTool) Name() string        { return "PersistMarker" }
func (turnTailMarkerTool) Description() string { return "append a durable turn marker" }
func (turnTailMarkerTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (turnTailMarkerTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (turnTailMarkerTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t turnTailMarkerTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	if t.poisonPath != "" {
		if err := os.Remove(t.poisonPath); err != nil {
			return nil, err
		}
		if err := os.Mkdir(t.poisonPath, 0o700); err != nil {
			return nil, err
		}
	}
	return &tools.Result{Output: "persisted marker"}, nil
}

type cwdCaptureProvider struct {
	activationTestProvider
	mu    sync.Mutex
	calls int
}

func (p *cwdCaptureProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		return &composerSummaryStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "capture-cwd-1", ToolName: "CaptureCwd"},
			{Type: "tool_input_delta", ToolUseID: "capture-cwd-1", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "capture-cwd-1", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &composerSummaryStream{events: []llm.StreamEvent{
		{Type: "message_stop", StopReason: "end_turn"},
	}}, nil
}

type cwdCaptureTool struct {
	tools.BaseTool
	mu  sync.Mutex
	cwd string
}

func (*cwdCaptureTool) Name() string        { return "CaptureCwd" }
func (*cwdCaptureTool) Description() string { return "capture the effective tool working directory" }
func (*cwdCaptureTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (*cwdCaptureTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*cwdCaptureTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t *cwdCaptureTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	t.mu.Lock()
	t.cwd = agent.CwdFromContext(ctx)
	t.mu.Unlock()
	return &tools.Result{Output: "captured"}, nil
}

func (t *cwdCaptureTool) captured() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

func testServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewServer("127.0.0.1:0", nil, store), store
}

func TestHealthAndSecurityHeaders(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q", got)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"build"`)) {
		t.Fatalf("health response missing build identity: %s", rr.Body.String())
	}
}

func TestNativeDesktopFrameRequiresLaunchToken(t *testing.T) {
	t.Setenv("METIS_DESKTOP_FRAME_TOKEN", "test-launch-token")
	s, _ := testServer(t)

	for _, tc := range []struct {
		path      string
		wantFrame string
	}{
		{path: "/", wantFrame: "DENY"},
		{path: "/?desktop-frame=wrong", wantFrame: "DENY"},
		{path: "/?desktop-frame=test-launch-token", wantFrame: ""},
		{path: "/api/health?desktop-frame=test-launch-token", wantFrame: "DENY"},
	} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := rr.Header().Get("X-Frame-Options"); got != tc.wantFrame {
			t.Errorf("GET %s X-Frame-Options = %q, want %q", tc.path, got, tc.wantFrame)
		}
	}
}

func TestStatusIncludesContextPressure(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.ContextWindow = 128_000
	loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), "model", loop.ContextWindow, provider)
	loop.ResetSession([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("context ", 200)}}}})
	s := NewServer("127.0.0.1:0", loop, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		ContextUsed      int     `json:"contextUsed"`
		ContextWindow    int     `json:"contextWindow"`
		CompactThreshold float64 `json:"compactThreshold"`
		Build            string  `json:"build"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ContextUsed <= 0 || payload.ContextWindow != 128_000 || payload.CompactThreshold <= 0 || payload.Build == "" {
		t.Fatalf("incomplete context status: %+v", payload)
	}
}

func TestTurnPersistsAutomaticCompactionHistoryReplacement(t *testing.T) {
	provider := &autoCompactPersistenceProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypassPermissions), nil, "system", 2)
	loop.Model = provider.ModelID()
	loop.ContextWindow = provider.MaxContextTokens()
	compactConfig := agent.DefaultCompactionConfig()
	compactConfig.Threshold = 0.01
	compactConfig.MinimumTokens = 0
	compactConfig.ProtectFirst = 1
	compactConfig.ProtectLast = 2
	compactConfig.SnipThreshold = 0
	compactConfig.CollapseThreshold = 0
	compactConfig.IdleMaxSeconds = 0
	compactConfig.MaxSummarizeInputTokens = 0
	loop.Compactor = agent.NewCompactor(compactConfig, provider.ModelID(), provider.MaxContextTokens(), provider)

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "auto-compact-persistence"
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: provider.Name(), Model: provider.ModelID(), System: "system", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		text := fmt.Sprintf("old turn %d %s", i, strings.Repeat("history ", 20))
		if i == 4 {
			text = "OLD_MIDDLE_SENTINEL " + text
		}
		if err := store.AppendMessage(id, llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	_, history, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	loop.Restore(history)
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: id,
		ProviderName:     provider.Name(),
	})

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
		`{"sessionId":"`+id+`","input":"latest request"}`,
	)))
	if rr.Code != http.StatusOK {
		t.Fatalf("turn = %d: %s", rr.Code, rr.Body.String())
	}

	live := loop.History()
	_, durable, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(durable, live) {
		t.Fatalf("durable history diverged after automatic compaction:\n durable=%#v\n live=%#v", durable, live)
	}
	rawLog, err := os.ReadFile(filepath.Join(store.Dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rawLog, []byte(`"type":"history_replace"`)) {
		t.Fatalf("automatic compaction did not persist a history replacement: %s", rawLog)
	}
	for _, message := range durable {
		for _, block := range message.Content {
			if strings.Contains(block.Text, "OLD_MIDDLE_SENTINEL") {
				t.Fatalf("summarized middle message revived in durable history: %#v", message)
			}
		}
	}
}

func TestSessionCreateListAndLoad(t *testing.T) {
	s, store := testServer(t)
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewBufferString(`{"model":"test-model"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rr.Code, rr.Body.String())
	}
	var created sessionItem
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Model != "test-model" {
		t.Fatalf("created = %+v", created)
	}
	if err := store.AppendMessage(created.ID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(created.ID)) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		Sessions []sessionItem `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || !listed.Sessions[0].CreatedAt.Equal(created.CreatedAt) || listed.Sessions[0].UpdatedAt.IsZero() {
		t.Fatalf("listed session timestamps = %+v, want stable createdAt plus updatedAt", listed.Sessions)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/"+created.ID, nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("hello")) {
		t.Fatalf("load: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSessionsListUsesStableCursorAndServerSideSearch(t *testing.T) {
	s, store := testServer(t)
	for i, title := range []string{"alpha report", "beta migration", "gamma notes"} {
		id := fmt.Sprintf("page-%d", i)
		if err := store.WriteHeaderFull(session.Header{ID: id, Model: "gpt-test", Title: title, WorkDir: filepath.Join("/workspace", title)}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: title}}}); err != nil {
			t.Fatal(err)
		}
	}
	h := s.handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions?limit=2", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("first page: %d %s", rr.Code, rr.Body.String())
	}
	var first struct {
		Sessions   []sessionItem `json:"sessions"`
		NextCursor string        `json:"nextCursor"`
		Total      int           `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 2 || first.NextCursor == "" || first.Total != 3 {
		t.Fatalf("first page = %+v", first)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions?limit=2&cursor="+first.NextCursor, nil))
	var second struct {
		Sessions []sessionItem `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil || len(second.Sessions) != 1 {
		t.Fatalf("second page: %d %s err=%v", rr.Code, rr.Body.String(), err)
	}
	seen := map[string]bool{}
	for _, item := range first.Sessions {
		seen[item.ID] = true
	}
	if seen[second.Sessions[0].ID] {
		t.Fatalf("cursor pages overlap on %q", second.Sessions[0].ID)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions?q=beta", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("beta migration")) || bytes.Contains(rr.Body.Bytes(), []byte("alpha report")) {
		t.Fatalf("server search: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSessionArchiveAPIIsRecoverableAndFiltered(t *testing.T) {
	s, store := testServer(t)
	if err := store.WriteHeader("archive-me", "model", "system"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("archive-me", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "keep me"}}}); err != nil {
		t.Fatal(err)
	}
	h := s.handler()

	archive := func(value bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"id": "archive-me", "archived": value})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/archive", bytes.NewReader(body)))
		return rr
	}
	if rr := archive(true); rr.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "archive-me.jsonl")); err != nil {
		t.Fatalf("archive removed transcript: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if bytes.Contains(rr.Body.Bytes(), []byte("archive-me")) {
		t.Fatalf("default list contains archived session: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions?archived_only=true", nil))
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"id":"archive-me"`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"archived":true`)) {
		t.Fatalf("archive list missing metadata: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/archive-me", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("keep me")) {
		t.Fatalf("archived session is not recoverable: %d %s", rr.Code, rr.Body.String())
	}

	if rr := archive(false); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if !bytes.Contains(rr.Body.Bytes(), []byte("archive-me")) {
		t.Fatalf("restored session missing from default list: %s", rr.Body.String())
	}
}

func TestSessionArchiveAPIRejectsUnsafeID(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/archive", bytes.NewBufferString(`{"id":"../escape","archived":true}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

func TestSessionDeleteAPIRemovesOwnedDataAndPreservesNeighbors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "delete-me"
	const neighbor = "delete-me-too"
	targetWorkDir := filepath.Join(home, "target-workspace")
	neighborWorkDir := filepath.Join(home, "neighbor-workspace")
	for _, id := range []string{target, neighbor} {
		workDir := targetWorkDir
		if id == neighbor {
			workDir = neighborWorkDir
		}
		if err := store.WriteHeaderFull(session.Header{ID: id, Model: "model", System: "system", WorkDir: workDir}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: id}}}); err != nil {
			t.Fatal(err)
		}
		store.NewTimingRecorder(id).Record("Read", time.Millisecond, false)
		if err := store.WriteCost(id, session.CostSnapshot{InputTokens: 1}); err != nil {
			t.Fatal(err)
		}
		if err := store.Snapshot(id, "named"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Archive(target); err != nil {
		t.Fatal(err)
	}
	if err := session.WritePointer(target, targetWorkDir); err != nil {
		t.Fatal(err)
	}
	if err := session.WritePointer(neighbor, neighborWorkDir); err != nil {
		t.Fatal(err)
	}

	mustWrite := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(home, "tasks", target+".json"), `{}`)
	mustWrite(filepath.Join(home, "tasks", neighbor+".json"), `{}`)
	mustWrite(filepath.Join(home, "sessions", target, "tasks-structured.json"), `{}`)
	mustWrite(filepath.Join(home, "sessions", neighbor, "tasks-structured.json"), `{}`)
	mustWrite(filepath.Join(home, ".metis", "checkpoints", target, "HEAD"), "target")
	mustWrite(filepath.Join(home, ".metis", "checkpoints", neighbor, "HEAD"), "neighbor")
	mustWrite(filepath.Join(home, "dump-prompts", target+".jsonl"), "target\n")
	mustWrite(filepath.Join(home, "dump-prompts", neighbor+".jsonl"), "neighbor\n")
	mustWrite(filepath.Join(store.Dir, "tags", target+".txt"), "private")
	mustWrite(filepath.Join(store.Dir, "tags", neighbor+".txt"), "keep")
	if _, err := rtpkg.SaveSnapshot(rtpkg.Snapshot{SessionID: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := rtpkg.SaveSnapshot(rtpkg.Snapshot{SessionID: neighbor}); err != nil {
		t.Fatal(err)
	}

	traceAdapter := rtpkg.InstallTrace(filepath.Join(home, "traces"))
	if traceAdapter == nil || rtpkg.CurrentTraceStore() == nil {
		t.Fatal("trace store was not installed")
	}
	if err := rtpkg.CurrentTraceStore().Append(&session.TraceEvent{SessionID: target, Kind: "text", Text: "private"}); err != nil {
		t.Fatal(err)
	}
	if err := rtpkg.CurrentTraceStore().Append(&session.TraceEvent{SessionID: neighbor, Kind: "text", Text: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := rtpkg.CurrentTraceStore().Sync(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rtpkg.CurrentTraceStore().Close() })

	s := NewServer("127.0.0.1:0", nil, store)
	s.hub.publish(target, agent.Event{Kind: agent.EventInfo, Info: "private replay"})
	s.hub.publish(neighbor, agent.Event{Kind: agent.EventInfo, Info: "keep replay"})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	var response struct {
		Deleted bool   `json:"deleted"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || !response.Deleted || response.ID != target {
		t.Fatalf("delete response = %+v, err=%v", response, err)
	}

	removed := []string{
		filepath.Join(store.Dir, target+".jsonl"),
		filepath.Join(store.Dir, target+".timing.jsonl"),
		filepath.Join(store.Dir, target+".cost.json"),
		filepath.Join(store.Dir, ".archive", target+".json"),
		filepath.Join(store.Dir, "snapshots", target+"-named.json"),
		filepath.Join(store.Dir, "tags", target+".txt"),
		filepath.Join(home, "tasks", target+".json"),
		filepath.Join(home, "sessions", target),
		filepath.Join(home, ".metis", "checkpoints", target),
		filepath.Join(home, "dump-prompts", target+".jsonl"),
		filepath.Join(home, "snapshots", target+".json"),
		filepath.Join(home, "traces", target+".jsonl"),
	}
	for _, path := range removed {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("deleted session path still exists: %s (err=%v)", path, err)
		}
	}
	kept := []string{
		filepath.Join(store.Dir, neighbor+".jsonl"),
		filepath.Join(store.Dir, neighbor+".timing.jsonl"),
		filepath.Join(store.Dir, neighbor+".cost.json"),
		filepath.Join(store.Dir, "snapshots", neighbor+"-named.json"),
		filepath.Join(store.Dir, "tags", neighbor+".txt"),
		filepath.Join(home, "tasks", neighbor+".json"),
		filepath.Join(home, "sessions", neighbor, "tasks-structured.json"),
		filepath.Join(home, ".metis", "checkpoints", neighbor, "HEAD"),
		filepath.Join(home, "dump-prompts", neighbor+".jsonl"),
		filepath.Join(home, "snapshots", neighbor+".json"),
		filepath.Join(home, "traces", neighbor+".jsonl"),
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("neighbor path was removed: %s (%v)", path, err)
		}
	}
	if events := rtpkg.CurrentTraceStore().Events(target); len(events) != 0 {
		t.Fatalf("deleted trace remained in memory: %+v", events)
	}
	if pointer, err := session.ReadPointer(targetWorkDir); err != nil || pointer != nil {
		t.Fatalf("deleted session recovery pointer = %+v, err=%v", pointer, err)
	}
	if pointer, err := session.ReadPointer(neighborWorkDir); err != nil || pointer == nil || pointer.SessionID != neighbor {
		t.Fatalf("neighbor recovery pointer = %+v, err=%v", pointer, err)
	}
	if len(s.hub.replay) != 1 || s.hub.replay[0].session != neighbor {
		t.Fatalf("SSE replay was not scrubbed exactly: %+v", s.hub.replay)
	}
}

func TestSessionDeletePreservesRecoveryPointerOwnedByAnotherSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "delete-old-session"
	const current = "keep-new-session"
	workDir := filepath.Join(home, "shared-workspace")
	if err := store.WriteHeaderFull(session.Header{ID: target, WorkDir: workDir}); err != nil {
		t.Fatal(err)
	}
	if err := session.WritePointer(current, workDir); err != nil {
		t.Fatal(err)
	}

	s := NewServer("127.0.0.1:0", nil, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	pointer, err := session.ReadPointer(workDir)
	if err != nil || pointer == nil || pointer.SessionID != current {
		t.Fatalf("newer recovery pointer was changed: pointer=%+v err=%v", pointer, err)
	}
}

func TestSessionDeleteRemovesOnlyAttributedMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "delete-memory-session"
	if err := store.WriteHeaderFull(session.Header{ID: target}); err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManager(filepath.Join(home, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveDailyNote(target, "desktop-switch", "owned daily fact"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveDailyNote("keep-memory-session", "desktop-switch", "shared daily fact"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Archival().Insert(memory.Passage{Content: "owned archive fact", SourceSessionID: target}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Archival().Insert(memory.Passage{Content: "shared archive fact", SourceSessionID: "keep-memory-session"}); err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Memory = manager
	s := NewServer("127.0.0.1:0", loop, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	notes, err := manager.ListDailyNotes(10)
	if err != nil || len(notes) != 1 || notes[0].SessionID != "keep-memory-session" {
		t.Fatalf("daily memory deletion crossed scope: notes=%+v err=%v", notes, err)
	}
	hits, err := manager.Archival().Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil || len(hits) != 1 || hits[0].SourceSessionID != "keep-memory-session" {
		t.Fatalf("archival memory deletion crossed scope: hits=%+v err=%v", hits, err)
	}
}

func TestSessionDeleteMemoryFailurePreservesTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "delete-memory-partial-failure"
	if err := store.WriteHeaderFull(session.Header{ID: target}); err != nil {
		t.Fatal(err)
	}
	managerRoot := filepath.Join(home, "memory")
	manager, err := memory.NewMemoryManager(managerRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Make one memory tier fail deterministically. DeleteSession may have
	// already removed rows from other tiers when it returns this error, so the
	// Desktop must leave the canonical transcript visible for a safe retry.
	dailyRoot := filepath.Join(managerRoot, "daily")
	if err := os.RemoveAll(dailyRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dailyRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Memory = manager
	s := NewServer("127.0.0.1:0", loop, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+target, nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want %d: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
	var response struct {
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || !response.Partial {
		t.Fatalf("partial delete response = %+v, err=%v", response, err)
	}
	if hdr, _, err := store.Load(target); err != nil || hdr == nil || hdr.ID != target {
		t.Fatalf("transcript disappeared after partial memory failure: header=%+v err=%v", hdr, err)
	}
}

func TestDesktopSessionSwitchPersistsVisibleDailyMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const current = "daily-current"
	const next = "daily-next"
	for _, id := range []string{current, next} {
		if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: "model", System: "system", Mode: "ask"}); err != nil {
			t.Fatal(err)
		}
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	manager, err := memory.NewMemoryManager(filepath.Join(home, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	loop.Memory = manager
	loop.Restore([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "text", Text: "remember the visible preference"},
			{Type: "text", Text: "<auto-retrieve>hidden old memory</auto-retrieve>", Synthetic: true},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "visible answer"}}},
	})
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: current,
		ProviderName:     "wire",
	})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/activate", bytes.NewBufferString(`{"id":"`+next+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rr.Code, rr.Body.String())
	}
	notes, err := manager.ListDailyNotes(10)
	if err != nil || len(notes) != 1 {
		t.Fatalf("daily notes=%+v err=%v", notes, err)
	}
	if notes[0].SessionID != current || notes[0].Source != "desktop-switch" ||
		!strings.Contains(notes[0].Summary, "visible preference") || !strings.Contains(notes[0].Summary, "visible answer") {
		t.Fatalf("daily note missing Desktop history metadata: %+v", notes[0])
	}
	if strings.Contains(notes[0].Summary, "hidden old memory") {
		t.Fatalf("synthetic recall leaked into daily memory: %+v", notes[0])
	}
}

func TestSummarizeMemoryHistoryKeepsRecentTail(t *testing.T) {
	history := make([]llm.Message, 0, 12)
	for i := 0; i < 11; i++ {
		history = append(history, llm.Message{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("OLD-MARKER-%02d %s", i, strings.Repeat("旧", 320)),
			}},
		})
	}
	history = append(history, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "RECENT-MARKER keep this newest fact"}},
	})

	summary := summarizeMemoryHistory(history)
	if !strings.Contains(summary, "RECENT-MARKER") {
		t.Fatalf("recent history was truncated: %q", summary)
	}
	if strings.Contains(summary, "OLD-MARKER-00") {
		t.Fatalf("summary retained the oldest prefix instead of the recent tail: %q", summary)
	}
	if !strings.HasPrefix(summary, "…") {
		t.Fatalf("truncated history should advertise the omitted prefix: %q", summary)
	}
}

func TestSessionDeleteActiveSessionCrossesToFreshRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "active-delete"
	if err := store.WriteHeaderFull(session.Header{ID: target, Provider: "wire", Model: "model", System: "system", Mode: "ask"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(target, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "delete this"}}}); err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	loop.Restore([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "delete this"}}}})
	boundaryCalls := 0
	switchedID := ""
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID:    target,
		ProviderName:        "wire",
		FreshPermissionMode: permission.ModeAsk,
		SessionBoundary:     func() { boundaryCalls++ },
		SessionSwitch:       func(id, _ string) { switchedID = id },
	})

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete active: %d %s", rr.Code, rr.Body.String())
	}
	var response struct {
		ActiveSessionID string `json:"activeSessionId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ActiveSessionID == "" || response.ActiveSessionID == target {
		t.Fatalf("replacement session id = %q", response.ActiveSessionID)
	}
	s.stateMu.RLock()
	activeID := s.activeSessionID
	s.stateMu.RUnlock()
	if activeID != response.ActiveSessionID || switchedID != response.ActiveSessionID || boundaryCalls != 1 {
		t.Fatalf("active reset incomplete: active=%q switched=%q boundary=%d response=%q", activeID, switchedID, boundaryCalls, response.ActiveSessionID)
	}
	if got := loop.History(); len(got) != 0 {
		t.Fatalf("replacement runtime retained deleted history: %+v", got)
	}
	if _, _, err := store.Load(target); !os.IsNotExist(err) {
		t.Fatalf("deleted active transcript still loads: %v", err)
	}
	if hdr, history, err := store.Load(response.ActiveSessionID); err != nil || hdr == nil || len(history) != 0 {
		t.Fatalf("replacement is not durable and empty: header=%+v history=%+v err=%v", hdr, history, err)
	}
}

func TestSessionDeleteRejectsUnknownUnsafeAndBusyTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	s, store := testServer(t)
	if err := store.WriteHeader("busy-delete", "model", "system"); err != nil {
		t.Fatal(err)
	}
	h := s.handler()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/sessions/missing-delete", http.StatusNotFound},
		{"/api/sessions/a/b", http.StatusBadRequest},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, tc.path, nil))
		if rr.Code != tc.want {
			t.Errorf("DELETE %s = %d, want %d: %s", tc.path, rr.Code, tc.want, rr.Body.String())
		}
	}

	s.runMu.Lock()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions/busy-delete", nil))
	s.runMu.Unlock()
	if rr.Code != http.StatusConflict {
		t.Fatalf("busy delete = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if _, _, err := store.Load("busy-delete"); err != nil {
		t.Fatalf("busy delete changed transcript: %v", err)
	}
}

func TestSessionCreatePersistsResumeCompleteRuntimeHeader(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "default-model"}
	gate := permission.New(permission.ModeAcceptEdits)
	loop := agent.NewLoop(provider, tools.NewRegistry(), gate, agent.NewHookRegistry(), "desktop-system", 2)
	loop.Model = "default-model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID:    "initial",
		ProviderName:        "desktop-profile",
		FreshPermissionMode: permission.ModeAcceptEdits,
	})

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewBufferString(`{"model":"chosen-model"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rr.Code, rr.Body.String())
	}
	var created sessionItem
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.LoadHeader(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Provider != "desktop-profile" || hdr.Model != "chosen-model" ||
		hdr.System != "desktop-system" || hdr.Mode != "acceptEdits" || hdr.WorkDir == "" {
		t.Fatalf("created session header is not resume-complete: %+v", hdr)
	}
}

func TestSessionAPIRejectsBadMethodsAndIDs(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodDelete, "/api/sessions", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/sessions/id", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/sessions/a/b", http.StatusBadRequest},
		{http.MethodGet, "/api/sessions/missing", http.StatusNotFound},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rr.Code, tc.want)
		}
	}
}

func TestTurnRequiresRuntimeAndInput(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"input":"hello"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestFailedTurnPersistsDurableSessionStatus(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	const id = "durable-turn-status"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: "model", System: "system", Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire", InitialSessionID: id})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"sessionId":"durable-turn-status","input":"hello"}`)))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("turn status = %d, want 502: %s", rr.Code, rr.Body.String())
	}
	hdr, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Status != "failed" {
		t.Fatalf("durable status = %q, want failed", hdr.Status)
	}
}

func TestFailedAndCanceledTurnsPersistHistoryTail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "provider error"},
		{name: "canceled follow-up", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			id := "persist-turn-tail-" + strings.ReplaceAll(tc.name, " ", "-")
			provider := &turnTailFailureProvider{blockOnSecond: tc.cancel}
			if tc.cancel {
				provider.secondStarted = make(chan struct{})
			}
			registry := tools.NewRegistry()
			registry.Register(turnTailMarkerTool{})
			loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
			loop.Model = provider.ModelID()
			if err := store.WriteHeaderFull(session.Header{
				ID: id, Provider: provider.Name(), Model: provider.ModelID(), System: "system", Status: "idle",
			}); err != nil {
				t.Fatal(err)
			}
			s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
				ProviderName: provider.Name(), InitialSessionID: id,
			})
			h := s.handler()
			turnResult := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
					`{"sessionId":"`+id+`","input":"persist this failed turn"}`,
				)))
				turnResult <- rr
			}()

			if tc.cancel {
				select {
				case <-provider.secondStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("follow-up request did not start")
				}
				stopRR := httptest.NewRecorder()
				h.ServeHTTP(stopRR, httptest.NewRequest(http.MethodPost, "/api/stop", nil))
				if stopRR.Code != http.StatusOK {
					t.Fatalf("stop status = %d: %s", stopRR.Code, stopRR.Body.String())
				}
			}

			var rr *httptest.ResponseRecorder
			select {
			case rr = <-turnResult:
			case <-time.After(3 * time.Second):
				t.Fatal("turn did not finish")
			}
			wantStatus := http.StatusBadGateway
			if tc.cancel {
				wantStatus = http.StatusOK
			}
			if rr.Code != wantStatus {
				t.Fatalf("turn status = %d, want %d: %s", rr.Code, wantStatus, rr.Body.String())
			}
			if tc.cancel && !bytes.Contains(rr.Body.Bytes(), []byte(`"stopped":true`)) {
				t.Fatalf("canceled turn missing clean stopped response: %s", rr.Body.String())
			}

			live := loop.History()
			_, durable, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(live) < 3 {
				t.Fatalf("test did not produce a post-checkpoint tool round: %#v", live)
			}
			if !reflect.DeepEqual(durable, live) {
				t.Fatalf("durable history diverged after failed turn:\n durable=%#v\n live=%#v", durable, live)
			}
		})
	}
}

func TestTurnPersistenceFailureTakesPriorityOverRunError(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "turn-persistence-error-priority"
	provider := &turnTailFailureProvider{}
	registry := tools.NewRegistry()
	registry.Register(turnTailMarkerTool{poisonPath: filepath.Join(store.Dir, id+".jsonl")})
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	loop.Model = provider.ModelID()
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: provider.Name(), Model: provider.ModelID(), System: "system", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		ProviderName: provider.Name(), InitialSessionID: id,
	})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
		`{"sessionId":"`+id+`","input":"force persistence failure"}`,
	)))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("turn status = %d, want 500: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "failed to persist") {
		t.Fatalf("500 response does not identify persistence failure: %s", rr.Body.String())
	}
}

func TestStopTargetsRunningSessionAndReturnsCleanStoppedTurn(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &statusTestProvider{
		activationTestProvider: activationTestProvider{name: "wire", model: "model"},
		started:                make(chan struct{}),
	}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	const id = "targeted-stop-session"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: "model", System: "system", Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire", InitialSessionID: id})
	h := s.handler()
	turnResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"sessionId":"`+id+`","input":"keep running"}`)))
		turnResult <- rr
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}

	status := httptest.NewRecorder()
	h.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"turnRunning":true`)) ||
		!bytes.Contains(status.Body.Bytes(), []byte(`"runningSessionId":"`+id+`"`)) {
		t.Fatalf("running status = %d: %s", status.Code, status.Body.String())
	}

	if err := store.WriteHeaderFull(session.Header{ID: "view-another-session", Provider: "wire", Model: "model", System: "system", Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	switchAttempt := httptest.NewRecorder()
	h.ServeHTTP(switchAttempt, httptest.NewRequest(http.MethodPost, "/api/sessions/activate", bytes.NewBufferString(`{"id":"view-another-session"}`)))
	if switchAttempt.Code != http.StatusConflict || !bytes.Contains(switchAttempt.Body.Bytes(), []byte(`"turnRunning":true`)) ||
		!bytes.Contains(switchAttempt.Body.Bytes(), []byte(`"runningSessionId":"`+id+`"`)) {
		t.Fatalf("running session activation conflict = %d: %s", switchAttempt.Code, switchAttempt.Body.String())
	}

	wrong := httptest.NewRecorder()
	h.ServeHTTP(wrong, httptest.NewRequest(http.MethodPost, "/api/stop", bytes.NewBufferString(`{"sessionId":"another-session"}`)))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong-session stop = %d, want 409: %s", wrong.Code, wrong.Body.String())
	}

	stop := httptest.NewRecorder()
	h.ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "/api/stop", bytes.NewBufferString(`{"sessionId":"`+id+`"}`)))
	if stop.Code != http.StatusOK || !bytes.Contains(stop.Body.Bytes(), []byte(`"stopped":true`)) {
		t.Fatalf("targeted stop = %d: %s", stop.Code, stop.Body.String())
	}

	select {
	case turn := <-turnResult:
		if turn.Code != http.StatusOK || !bytes.Contains(turn.Body.Bytes(), []byte(`"stopped":true`)) {
			t.Fatalf("canceled turn = %d, want clean stopped response: %s", turn.Code, turn.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("targeted stop did not finish the turn")
	}
}

func TestCompletedAndStoppedTurnsPersistDurableSessionStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
		want string
	}{
		{name: "completed", want: "completed"},
		{name: "stopped", stop: true, want: "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			provider := &statusTestProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
			if tc.stop {
				provider.started = make(chan struct{})
			}
			loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
			loop.Model = "model"
			id := "durable-turn-status-" + tc.name
			if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: "model", System: "system", Status: "idle"}); err != nil {
				t.Fatal(err)
			}
			s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "wire", InitialSessionID: id})
			h := s.handler()
			turnResult := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"sessionId":"`+id+`","input":"hello"}`)))
				turnResult <- rr
			}()
			if tc.stop {
				select {
				case <-provider.started:
				case <-time.After(2 * time.Second):
					t.Fatal("turn did not start")
				}
				stopRR := httptest.NewRecorder()
				h.ServeHTTP(stopRR, httptest.NewRequest(http.MethodPost, "/api/stop", nil))
				if stopRR.Code != http.StatusOK {
					t.Fatalf("stop status = %d: %s", stopRR.Code, stopRR.Body.String())
				}
			}
			select {
			case <-turnResult:
			case <-time.After(3 * time.Second):
				t.Fatal("turn did not finish")
			}
			hdr, _, err := store.LoadHeader(id)
			if err != nil {
				t.Fatal(err)
			}
			if hdr.Status != tc.want {
				t.Fatalf("durable status = %q, want %q", hdr.Status, tc.want)
			}
		})
	}
}

func TestTurnRejectsUnsafeSessionIDBeforeSidecarRouting(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "profile",
	})

	for _, id := range []string{"../target", `..\target`, ".", "..", "target\nother"} {
		t.Run(id, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"sessionId": id, "input": "hello"})
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewReader(body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("session id %q status = %d, want 400: %s", id, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUnsafeAPIsRejectCrossOriginBrowserRequests(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "origin", header: "Origin", value: "https://attacker.example"},
		{name: "fetch metadata", header: "Sec-Fetch-Site", value: "cross-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/sessions", bytes.NewBufferString(`{"model":"test"}`))
			req.Header.Set(tc.header, tc.value)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUnsafeAPIsAllowSameOriginAndNonBrowserClients(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	for _, origin := range []string{"", "http://127.0.0.1:8080"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/sessions", bytes.NewBufferString(`{"model":"test"}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("origin %q status = %d, want 201: %s", origin, rr.Code, rr.Body.String())
		}
	}
}

func TestActivateSessionRestoresHeaderStateAndRebindsSidecars(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{
		ID: "source", Provider: "source-profile", Model: "source-model",
		System: "source-system", Mode: "ask",
	}); err != nil {
		t.Fatal(err)
	}
	targetHeader := session.Header{
		ID: "target", Provider: "target-profile", Model: "target-model",
		System: "target-system", Mode: "plan",
		AlwaysAllow: []session.SavedRule{{
			Tool: "Read", Match: "README", Verb: int(permission.DecisionAllow), Source: "policy:forged",
		}},
	}
	if err := store.WriteHeaderFull(targetHeader); err != nil {
		t.Fatal(err)
	}
	targetHistory := []llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "target history"}},
	}}
	if err := store.AppendMessage("target", targetHistory[0]); err != nil {
		t.Fatal(err)
	}

	sourceProvider := &activationTestProvider{name: "source-wire", model: "source-model"}
	targetProvider := &activationTestProvider{name: "target-wire", model: "target-model"}
	gate := permission.New(permission.ModeAsk)
	gate.AppendRules(
		permission.Rule{Tool: "Glob", Verb: permission.DecisionAllow, Source: "config:allow"},
		permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"},
	)
	loop := agent.NewLoop(sourceProvider, tools.NewRegistry(), gate, agent.NewHookRegistry(), "source-system", 2)
	loop.Model = "source-model"
	loop.SystemSections = []llm.SystemSection{{Name: "source", Body: "source-system"}}
	loop.Restore([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "source history"}}}})
	gate.SetModeChangeListener(func(mode permission.Mode) { loop.SetPlanMode(mode == permission.ModePlan) })

	var buildProviderName, buildModel, switchedID string
	boundaryCalls := 0
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID:    "source",
		ProviderName:        "source-profile",
		FreshPermissionMode: permission.ModeAsk,
		BuildProvider: func(providerName, model string) (*rtpkg.ProviderBuild, error) {
			buildProviderName, buildModel = providerName, model
			return &rtpkg.ProviderBuild{Provider: targetProvider, Model: targetProvider.model}, nil
		},
		SessionBoundary: func() { boundaryCalls++ },
		SessionSwitch:   func(id, _ string) { switchedID = id },
	})

	hdr, history, err := store.Load("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.activateSession("target", hdr, history); err != nil {
		t.Fatalf("activateSession: %v", err)
	}

	if buildProviderName != "target-profile" || buildModel != "target-model" {
		t.Fatalf("provider preflight = %q/%q", buildProviderName, buildModel)
	}
	if loop.Provider != targetProvider || loop.Model != "target-model" || loop.ContextWindow != 128_000 {
		t.Fatalf("provider/model not activated: provider=%T model=%q window=%d", loop.Provider, loop.Model, loop.ContextWindow)
	}
	if loop.System != "target-system" || len(loop.SystemSections) != 0 {
		t.Fatalf("system state not restored: system=%q sections=%+v", loop.System, loop.SystemSections)
	}
	if gate.Mode() != permission.ModePlan || !loop.IsPlanMode() {
		t.Fatalf("permission mode not restored: gate=%q plan=%v", gate.Mode(), loop.IsPlanMode())
	}
	rules := gate.Snapshot()
	if len(rules) != 2 || rules[0].Source != "config:allow" || rules[1].Source != "session:resumed(policy:forged)" {
		t.Fatalf("permission rules not crossed safely: %+v", rules)
	}
	if got := loop.History(); len(got) != 1 || got[0].Content[0].Text != "target history" {
		t.Fatalf("target transcript not restored: %+v", got)
	}
	if boundaryCalls != 1 || switchedID != "target" {
		t.Fatalf("session callbacks: boundary=%d switch=%q", boundaryCalls, switchedID)
	}

	loop.TimingSink("Read", 3*time.Millisecond, false)
	steps, err := store.ReadTiming("target")
	if err != nil || len(steps) != 1 || steps[0].Tool != "Read" {
		t.Fatalf("timing sidecar not rebound: steps=%+v err=%v", steps, err)
	}
	if oldSteps, err := store.ReadTiming("source"); err != nil || len(oldSteps) != 0 {
		t.Fatalf("timing leaked to source session: steps=%+v err=%v", oldSteps, err)
	}

	persistedSource, _, err := store.LoadHeader("source")
	if err != nil {
		t.Fatal(err)
	}
	if persistedSource.Provider != "source-profile" || persistedSource.Model != "source-model" ||
		persistedSource.System != "source-system" || len(persistedSource.AlwaysAllow) != 1 ||
		persistedSource.AlwaysAllow[0].Tool != "Edit" {
		t.Fatalf("source session state not persisted before switch: %+v", persistedSource)
	}
}

func TestActivateSessionRebuildsPersistedManagedDefaultPrompt(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	promptSections := rtpkg.AssembleSystemPromptSectionsCtx(rtpkg.PromptCtx{
		ProviderName: "wire",
		Model:        "model",
		EnabledTools: map[string]bool{"Read": true},
	}, rtpkg.AssembleOptions{SkipEnv: true})
	freshSystem := rtpkg.RenderSections(promptSections)
	loopSections := make([]llm.SystemSection, 0, len(promptSections))
	for _, section := range promptSections {
		loopSections = append(loopSections, llm.SystemSection{
			Name: section.Name, Body: section.Body, Cache: section.Cache, Volatile: section.Volatile,
		})
	}
	for _, hdr := range []session.Header{
		{ID: "source-managed", Provider: "wire", Model: "model", System: freshSystem, SystemPromptKind: session.SystemPromptKindDefault},
		{ID: "target-managed", Provider: "wire", Model: "model", System: "stale default with WebSearch", SystemPromptKind: session.SystemPromptKindDefault},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, freshSystem, 2)
	loop.Model = "model"
	loop.SystemSections = loopSections
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source-managed", ProviderName: "wire", FreshPermissionMode: permission.ModeDefault,
	})
	target, history, err := store.Load("target-managed")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.activateSession(target.ID, target, history); err != nil {
		t.Fatal(err)
	}
	if loop.System != freshSystem || strings.Contains(loop.System, "stale default") {
		t.Fatalf("managed prompt was not rebuilt from fresh typed sections:\n%s", loop.System)
	}
	if len(loop.SystemSections) == 0 {
		t.Fatal("managed prompt activation lost typed sections")
	}
}

func TestSessionViewDuringActiveTurnDoesNotRebindScopeUntilActivation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	allowed := rtpkg.NewAllowedDirs(nil)
	if err := allowed.RebindCWD(sourceDir); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeDefault)
	gate.SetPathScopeHook(allowed.Contains)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "scope-source", Provider: "wire", Model: "model", WorkDir: sourceDir, Mode: string(permission.ModeDefault)},
		{ID: "scope-target", Provider: "wire", Model: "model", WorkDir: targetDir, Mode: string(permission.ModeDefault)},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), gate, nil, "system", 2)
	loop.Model = "model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "scope-source", ProviderName: "wire", FreshPermissionMode: permission.ModeDefault,
		SessionSwitch: func(_ string, workDir string) {
			if err := allowed.RebindCWD(workDir); err != nil {
				t.Fatalf("rebind scope: %v", err)
			}
		},
	})

	s.runMu.Lock() // model an active turn owned by the source session
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/scope-target", nil))
	s.runMu.Unlock()
	if rr.Code != http.StatusOK {
		t.Fatalf("read-only session view = %d: %s", rr.Code, rr.Body.String())
	}
	if !allowed.Contains(filepath.Join(sourceDir, "source.txt")) || allowed.Contains(filepath.Join(targetDir, "target.txt")) {
		t.Fatalf("read-only view changed live scope: %v", allowed.Scope())
	}

	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/activate", bytes.NewBufferString(`{"id":"scope-target"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("activate target = %d: %s", rr.Code, rr.Body.String())
	}
	if !allowed.Contains(filepath.Join(targetDir, "target.txt")) || allowed.Contains(filepath.Join(sourceDir, "source.txt")) {
		t.Fatalf("activation did not atomically move live scope: %v", allowed.Scope())
	}
}

func TestActivateSessionWorkspacePreflightFailureLeavesSourceStateAtomic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	sourceDir := filepath.Join(home, "source-workspace")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	missingTargetDir := filepath.Join(home, "deleted-target-workspace")
	allowed := rtpkg.NewAllowedDirs(nil)
	if err := allowed.RebindCWD(sourceDir); err != nil {
		t.Fatal(err)
	}

	memoryManager, err := memory.NewMemoryManagerForWorkspace(filepath.Join(home, "memory"), sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryManager.Archival().Insert(memory.Passage{Content: "source-memory-sentinel", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "atomic-source", Provider: "wire", Model: "model", System: "system", WorkDir: sourceDir},
		{ID: "atomic-target", Provider: "wire", Model: "model", System: "system", WorkDir: missingTargetDir},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	sourceMessage := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "source-history-sentinel"}}}
	targetMessage := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "target-history-must-not-load"}}}
	if err := store.AppendMessage("atomic-source", sourceMessage); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("atomic-target", targetMessage); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeDefault)
	gate.SetPathScopeHook(allowed.Contains)
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = memoryManager
	loop.Restore([]llm.Message{sourceMessage})
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "atomic-source",
		ProviderName:     "wire",
		PrepareSessionSwitch: func(_ string, workDir string) (string, func(), error) {
			prepared, prepareErr := allowed.PrepareRebindCWD(workDir)
			if prepareErr != nil {
				return "", nil, prepareErr
			}
			return prepared.CanonicalPath(), prepared.Commit, nil
		},
	})
	targetHeader, targetHistory, err := store.Load("atomic-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.activateSession("atomic-target", targetHeader, targetHistory); err == nil || !strings.Contains(err.Error(), "workspace permission preflight") {
		t.Fatalf("activation error = %v, want workspace preflight failure", err)
	}

	server.stateMu.RLock()
	activeID, activeWorkDir := server.activeSessionID, server.activeWorkDir
	server.stateMu.RUnlock()
	if activeID != "atomic-source" || activeWorkDir != sourceDir {
		t.Fatalf("failed activation changed active state: id=%q workDir=%q", activeID, activeWorkDir)
	}
	if history := loop.History(); len(history) != 1 || history[0].Content[0].Text != "source-history-sentinel" {
		t.Fatalf("failed activation changed history: %+v", history)
	}
	if !allowed.Contains(filepath.Join(sourceDir, "still-authorized.txt")) || allowed.Contains(filepath.Join(missingTargetDir, "must-not-authorize.txt")) {
		t.Fatalf("failed activation changed allowed roots: %v", allowed.Scope())
	}
	if hits := loop.Memory.SearchCandidates("source-memory-sentinel", 5); len(hits) == 0 {
		t.Fatal("failed activation changed source workspace memory binding")
	}
}

func TestActivateSessionUsesGateModeAfterListenerDowngrade(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{ID: "source", Provider: "wire", Model: "model", Mode: string(permission.ModeDefault)}); err != nil {
		t.Fatal(err)
	}
	target := &session.Header{
		ID: "target-plan-downgrade", Provider: "wire", Model: "model",
		Mode: string(permission.ModePlan), PrePlanMode: string(permission.ModeDefault),
	}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}

	gate := permission.New(permission.ModeDefault)
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), gate, nil, "system", 2)
	loop.Model = "model"
	gate.SetModeChangeListener(func(mode permission.Mode) {
		loop.SetPlanMode(mode == permission.ModePlan)
		if mode == permission.ModePlan {
			gate.SetMode(permission.ModeDontAsk)
		}
	})
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source", ProviderName: "wire", FreshPermissionMode: permission.ModeDefault,
	})

	if err := s.activateSession(target.ID, target, nil); err != nil {
		t.Fatal(err)
	}
	if gate.Mode() != permission.ModeDontAsk || loop.IsPlanMode() {
		t.Fatalf("restored permission state diverged: gate=%q plan=%v", gate.Mode(), loop.IsPlanMode())
	}
	if got := loop.PrePlanMode(); got != "" {
		t.Fatalf("failed-closed switch retained pre-plan lineage %q", got)
	}
}

func TestActivateSessionRebindsWorkspaceMemoryAcrossLoopToolAndAutoMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	workspaceA := filepath.Join(home, "workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(home, "memory")
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := memory.NewMemoryManagerForWorkspace(root, workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if err := managerA.Archival().Insert(memory.Passage{Content: "alpha-only-recall", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	if err := managerB.Archival().Insert(memory.Passage{Content: "beta-only-recall", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceA},
		{ID: "target", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceB},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}

	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, managerA))
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, registry, gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = managerA
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{WorkingDirectory: workspaceA}
	}
	switchedWorkDir := ""
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
		SessionSwitch: func(_, workDir string) {
			switchedWorkDir = workDir
		},
	})
	autoMemoryJoined := false
	server.waitAutoMemoryIdle = func(context.Context) error {
		autoMemoryJoined = true
		return nil
	}

	hdr, history, err := store.Load("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.activateSession("target", hdr, history); err != nil {
		t.Fatalf("activate target: %v", err)
	}
	if !autoMemoryJoined {
		t.Fatal("workspace switch did not join the source Auto Memory extractor before rebinding")
	}
	if state := loop.CurrentStateSnapshot(); state.WorkingDirectory != workspaceB {
		t.Fatalf("runtime workspace = %q, want target %q", state.WorkingDirectory, workspaceB)
	}
	if switchedWorkDir != workspaceB {
		t.Fatalf("session switch workDir = %q, want target %q", switchedWorkDir, workspaceB)
	}
	if hits := loop.Memory.SearchCandidates("beta-only-recall", 10); len(hits) == 0 {
		t.Fatal("target workspace memory was not recalled after session activation")
	}
	for _, hit := range loop.Memory.SearchCandidates("alpha-only-recall", 10) {
		if strings.Contains(hit.Content, "alpha-only-recall") {
			t.Fatalf("source workspace memory leaked after activation: %+v", hit)
		}
	}

	memoryTool, ok := registry.Get("Memory")
	if !ok {
		t.Fatal("Memory tool disappeared during workspace activation")
	}
	result, err := memoryTool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "working", "content": "beta-tool-write",
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Memory tool write after activation: result=%+v err=%v", result, err)
	}
	blockB, err := managerB.ReadCoreBlock("working")
	if err != nil || !strings.Contains(blockB.Content, "beta-tool-write") {
		t.Fatalf("Memory tool did not write through target workspace repository: block=%+v err=%v", blockB, err)
	}
	blockA, err := managerA.ReadCoreBlock("working")
	if err == nil && strings.Contains(blockA.Content, "beta-tool-write") {
		t.Fatalf("Memory tool write leaked into source workspace: %+v", blockA)
	}
}

func TestWorkspaceSwitchWaitsForBackgroundAgentMemoryWriter(t *testing.T) {
	home := t.TempDir()
	workspaceA := filepath.Join(home, "workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(home, "memory")
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := memory.NewMemoryManagerForWorkspace(root, workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceA},
		{ID: "target", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceB},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}

	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, managerA))
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, registry, gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = managerA
	roster := agent.NewRoster(2)
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
		Roster:           roster,
	})
	memoryTool, ok := registry.Get("Memory")
	if !ok {
		t.Fatal("Memory tool unavailable after workspace binding")
	}

	canceled := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	var cancelOnce sync.Once
	teammate := &agent.Teammate{
		Name: "source-memory-writer",
		Cancel: func() {
			cancelOnce.Do(func() { close(canceled) })
		},
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-canceled
		<-releaseWriter
		result, executeErr := memoryTool.Execute(context.Background(), map[string]any{
			"action": "archive", "content": "source-agent-after-cancel", "memory_type": "project",
		})
		if executeErr == nil && (result == nil || result.IsError) {
			executeErr = fmt.Errorf("memory result: %+v", result)
		}
		roster.UnregisterTeammate(teammate)
		writerDone <- executeErr
	}()

	hdr, history, err := store.Load("target")
	if err != nil {
		t.Fatal(err)
	}
	activateDone := make(chan error, 1)
	go func() { activateDone <- server.activateSession("target", hdr, history) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("workspace switch did not cancel the source background agent")
	}
	select {
	case err := <-activateDone:
		t.Fatalf("workspace switch committed before source writer exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseWriter)
	if err := <-writerDone; err != nil {
		t.Fatalf("source background writer: %v", err)
	}
	if err := <-activateDone; err != nil {
		t.Fatalf("activate target: %v", err)
	}
	if hits := managerA.SearchCandidates("source-agent-after-cancel", 10); len(hits) == 0 {
		t.Fatal("canceled source writer did not finish against source workspace")
	}
	for _, hit := range managerB.SearchCandidates("source-agent-after-cancel", 10) {
		if strings.Contains(hit.Content, "source-agent-after-cancel") {
			t.Fatalf("source background writer leaked into target workspace: %+v", hit)
		}
	}
}

func TestSameWorkspaceSwitchKeepsCanceledAgentMemoryProvenance(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewMemoryManagerForWorkspace(filepath.Join(home, "memory"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err := store.WriteHeaderFull(session.Header{
			ID: id, Provider: "wire", Model: "model", System: "system", WorkDir: workspace,
		}); err != nil {
			t.Fatal(err)
		}
	}
	oldSessionID := rtpkg.CurrentSessionID()
	rtpkg.SetCurrentSessionID("source")
	t.Cleanup(func() { rtpkg.SetCurrentSessionID(oldSessionID) })

	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, manager))
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, registry, gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = manager
	roster := agent.NewRoster(2)
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
		Roster:           roster,
		SessionSwitch: func(id, _ string) {
			rtpkg.SetCurrentSessionID(id)
		},
	})
	memoryTool, ok := registry.Get("Memory")
	if !ok {
		t.Fatal("Memory tool unavailable after binding")
	}
	canceled := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	var cancelOnce sync.Once
	teammate := &agent.Teammate{Name: "source-provenance-writer", Cancel: func() {
		cancelOnce.Do(func() { close(canceled) })
	}}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-canceled
		<-releaseWriter
		result, executeErr := memoryTool.Execute(context.Background(), map[string]any{
			"action": "archive", "content": "same-workspace-source-fact", "memory_type": "project",
		})
		if executeErr == nil && (result == nil || result.IsError) {
			executeErr = fmt.Errorf("memory result: %+v", result)
		}
		roster.UnregisterTeammate(teammate)
		writerDone <- executeErr
	}()

	hdr, history, err := store.Load("target")
	if err != nil {
		t.Fatal(err)
	}
	activateDone := make(chan error, 1)
	go func() { activateDone <- server.activateSession("target", hdr, history) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("same-workspace switch did not cancel source agent")
	}
	select {
	case err := <-activateDone:
		t.Fatalf("same-workspace switch changed provenance before writer exit: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseWriter)
	if err := <-writerDone; err != nil {
		t.Fatalf("source provenance writer: %v", err)
	}
	if err := <-activateDone; err != nil {
		t.Fatalf("activate target: %v", err)
	}
	if got := rtpkg.CurrentSessionID(); got != "target" {
		t.Fatalf("target provenance router = %q", got)
	}
	found := false
	for _, hit := range manager.SearchCandidates("same-workspace-source-fact", 10) {
		if strings.Contains(hit.Content, "same-workspace-source-fact") {
			found = true
			if hit.SourceSessionID != "source" {
				t.Fatalf("late source fact attributed to %q, want source", hit.SourceSessionID)
			}
		}
	}
	if !found {
		t.Fatal("same-workspace source fact was not archived")
	}
}

func TestTurnToolsUseActivatedSessionWorkspace(t *testing.T) {
	home := t.TempDir()
	workspaceA := filepath.Join(home, "workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceA},
		{ID: "target", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceB},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}

	provider := &cwdCaptureProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
	capture := &cwdCaptureTool{}
	registry := tools.NewRegistry()
	registry.Register(capture)
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	loop.Model = "model"
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
	})

	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
		`{"sessionId":"target","input":"capture cwd"}`,
	)))
	if rr.Code != http.StatusOK {
		t.Fatalf("turn status = %d: %s", rr.Code, rr.Body.String())
	}
	if got := capture.captured(); got != workspaceB {
		t.Fatalf("tool cwd = %q, want activated session workspace %q", got, workspaceB)
	}
}

func TestTurnPublishesPreparedCanonicalWorkspaceWhenAliasRetargets(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "source")
	workspaceA := filepath.Join(home, "workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	for _, dir := range []string{source, workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(home, "workspace-link")
	if err := os.Symlink(workspaceA, alias); err != nil {
		t.Fatal(err)
	}
	wantCanonical, err := filepath.EvalSymlinks(workspaceA)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "wire", Model: "model", System: "system", WorkDir: source},
		{ID: "target", Provider: "wire", Model: "model", System: "system", WorkDir: alias},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}

	allowed := rtpkg.NewAllowedDirs(nil)
	if err := allowed.RebindCWD(source); err != nil {
		t.Fatal(err)
	}
	provider := &cwdCaptureProvider{activationTestProvider: activationTestProvider{name: "wire", model: "model"}}
	capture := &cwdCaptureTool{}
	registry := tools.NewRegistry()
	registry.Register(capture)
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	loop.Model = "model"
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
		PrepareSessionSwitch: func(sessionID, workDir string) (string, func(), error) {
			prepared, prepareErr := allowed.PrepareRebindCWD(workDir)
			if prepareErr != nil {
				return "", nil, prepareErr
			}
			if sessionID == "target" {
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(workspaceB, alias); err != nil {
					t.Fatal(err)
				}
			}
			return prepared.CanonicalPath(), prepared.Commit, nil
		},
	})

	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
		`{"sessionId":"target","input":"capture cwd"}`,
	)))
	if rr.Code != http.StatusOK {
		t.Fatalf("turn status = %d: %s", rr.Code, rr.Body.String())
	}
	if got := capture.captured(); got != wantCanonical {
		t.Fatalf("tool cwd = %q, want prepared canonical workspace %q", got, wantCanonical)
	}
	server.stateMu.RLock()
	activeWorkDir := server.activeWorkDir
	server.stateMu.RUnlock()
	if activeWorkDir != wantCanonical {
		t.Fatalf("activeWorkDir = %q, want prepared canonical workspace %q", activeWorkDir, wantCanonical)
	}
	if !allowed.Contains(filepath.Join(workspaceA, "a.txt")) || allowed.Contains(filepath.Join(workspaceB, "b.txt")) {
		t.Fatalf("allowed roots followed retargeted alias: %v", allowed.Scope())
	}
}

func TestNewServerBindsInitialHeaderWorkspaceBeforeFirstTurn(t *testing.T) {
	home := t.TempDir()
	workspaceA := filepath.Join(home, "launch-cwd-a")
	workspaceB := filepath.Join(home, "restored-session-b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(home, "memory")
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := memory.NewMemoryManagerForWorkspace(root, workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if err := managerA.Archival().Insert(memory.Passage{Content: "startup-alpha-only", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	if err := managerB.Archival().Insert(memory.Passage{Content: "startup-beta-only", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{
		ID: "restored", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceB,
	}); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, managerA))
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, registry, gate, nil, "system", 2)
	loop.Memory = managerA
	initialSwitchWorkDir := ""
	_ = NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "restored",
		ProviderName:     "wire",
		SessionSwitch: func(_, workDir string) {
			initialSwitchWorkDir = workDir
		},
	})
	if initialSwitchWorkDir != workspaceB {
		t.Fatalf("initial session switch workDir = %q, want %q", initialSwitchWorkDir, workspaceB)
	}

	if hits := loop.Memory.SearchCandidates("startup-beta-only", 10); len(hits) == 0 {
		t.Fatal("initial restored workspace memory was not bound before the first turn")
	}
	for _, hit := range loop.Memory.SearchCandidates("startup-alpha-only", 10) {
		if strings.Contains(hit.Content, "startup-alpha-only") {
			t.Fatalf("launch cwd memory leaked into restored initial session: %+v", hit)
		}
	}
	memoryTool, ok := registry.Get("Memory")
	if !ok {
		t.Fatal("Memory tool unavailable after initial workspace binding")
	}
	result, err := memoryTool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "working", "content": "startup-beta-tool-write",
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("initial workspace Memory write: result=%+v err=%v", result, err)
	}
	blockB, err := managerB.ReadCoreBlock("working")
	if err != nil || !strings.Contains(blockB.Content, "startup-beta-tool-write") {
		t.Fatalf("initial Memory tool did not target restored workspace: block=%+v err=%v", blockB, err)
	}
}

func TestActivateLegacyEmptyWorkDirFallsBackToLaunchWorkspace(t *testing.T) {
	home := t.TempDir()
	workspaceA := filepath.Join(home, "launch-workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspaceA); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWorkDir) }()

	root := filepath.Join(home, "memory")
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := memory.NewMemoryManagerForWorkspace(root, workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if err := managerA.Archival().Insert(memory.Passage{Content: "legacy-fallback-alpha", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	if err := managerB.Archival().Insert(memory.Passage{Content: "legacy-fallback-beta", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceA},
		{ID: "workspace-b", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceB},
		{ID: "legacy-empty", Provider: "wire", Model: "model", System: "system"},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, managerA))
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, registry, gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = managerA
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
	})
	for _, id := range []string{"workspace-b", "legacy-empty"} {
		hdr, history, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.activateSession(id, hdr, history); err != nil {
			t.Fatalf("activate %s: %v", id, err)
		}
	}
	if hits := loop.Memory.SearchCandidates("legacy-fallback-alpha", 10); len(hits) == 0 {
		t.Fatal("legacy empty WorkDir did not fall back to the launch workspace")
	}
	for _, hit := range loop.Memory.SearchCandidates("legacy-fallback-beta", 10) {
		if strings.Contains(hit.Content, "legacy-fallback-beta") {
			t.Fatalf("previous workspace remained bound for legacy empty WorkDir: %+v", hit)
		}
	}
}

func TestNewServerInitialWorkspaceRebindFailureFailsClosed(t *testing.T) {
	home := t.TempDir()
	workspaceA := filepath.Join(home, "workspace-a")
	if err := os.MkdirAll(workspaceA, 0o700); err != nil {
		t.Fatal(err)
	}
	badWorkspace := filepath.Join(home, "workspace-loop")
	if err := os.Symlink(badWorkspace, badWorkspace); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "memory")
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	if err := managerA.Archival().Insert(memory.Passage{Content: "must-not-leak-from-a", Type: memory.TypeProject}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{
		ID: "restored", Provider: "wire", Model: "model", System: "system", WorkDir: badWorkspace,
	}); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, managerA))
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, registry, gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = managerA
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "restored",
		ProviderName:     "wire",
	})
	hdr, history, err := store.Load("restored")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.activateSession("restored", hdr, history); err == nil || !strings.Contains(err.Error(), "memory workspace") {
		t.Fatalf("initial workspace binding error was not propagated: %v", err)
	}
	for _, hit := range loop.Memory.SearchCandidates("must-not-leak-from-a", 10) {
		if strings.Contains(hit.Content, "must-not-leak-from-a") {
			t.Fatalf("failed initial binding leaked the launch repository: %+v", hit)
		}
	}
	memoryTool, ok := registry.Get("Memory")
	if !ok {
		t.Fatal("Memory tool unavailable after fail-closed binding")
	}
	result, err := memoryTool.Execute(context.Background(), map[string]any{
		"action": "read", "target": "working",
	})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "unavailable") {
		t.Fatalf("failed initial binding did not disable Memory tool: result=%+v err=%v", result, err)
	}
	archiveResult, err := memoryTool.Execute(context.Background(), map[string]any{
		"action": "archive", "content": "must-not-report-success", "memory_type": "project",
	})
	if err != nil || archiveResult == nil || !archiveResult.IsError || !strings.Contains(archiveResult.Output, "unavailable") {
		t.Fatalf("failed initial binding archive did not fail closed: result=%+v err=%v", archiveResult, err)
	}
}

func TestActivateSessionMemoryJoinFailureKeepsSourceWorkspaceBound(t *testing.T) {
	home := t.TempDir()
	workspaceA := filepath.Join(home, "workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(home, "memory")
	managerA, err := memory.NewMemoryManagerForWorkspace(root, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := memory.NewMemoryManagerForWorkspace(root, workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceA},
		{ID: "target", Provider: "wire", Model: "model", System: "system", WorkDir: workspaceB},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewMemory(gate, managerA))
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, registry, gate, nil, "system", 2)
	loop.Model = "model"
	loop.Memory = managerA
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "wire",
	})
	waitErr := errors.New("auto memory still writing")
	server.waitAutoMemoryIdle = func(context.Context) error { return waitErr }
	hdr, history, err := store.Load("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.activateSession("target", hdr, history); !errors.Is(err, waitErr) {
		t.Fatalf("activate error = %v, want %v", err, waitErr)
	}

	memoryTool, ok := registry.Get("Memory")
	if !ok {
		t.Fatal("Memory tool unavailable after failed activation")
	}
	result, err := memoryTool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "working", "content": "source-after-failed-switch",
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("source Memory tool after failed activation: result=%+v err=%v", result, err)
	}
	blockA, err := managerA.ReadCoreBlock("working")
	if err != nil || !strings.Contains(blockA.Content, "source-after-failed-switch") {
		t.Fatalf("source workspace was no longer bound: block=%+v err=%v", blockA, err)
	}
	blockB, err := managerB.ReadCoreBlock("working")
	if err == nil && strings.Contains(blockB.Content, "source-after-failed-switch") {
		t.Fatalf("failed activation leaked Memory tool write into target workspace: %+v", blockB)
	}
}

func TestActivateSessionProviderFailureLeavesCurrentSessionUntouched(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []session.Header{
		{ID: "source", Provider: "source-profile", Model: "source-model", System: "source-system", Mode: "ask"},
		{ID: "target", Provider: "missing-profile", Model: "target-model", System: "target-system", Mode: "bypass"},
	} {
		if err := store.WriteHeaderFull(hdr); err != nil {
			t.Fatal(err)
		}
	}
	targetMessage := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "target"}}}
	if err := store.AppendMessage("target", targetMessage); err != nil {
		t.Fatal(err)
	}

	sourceProvider := &activationTestProvider{name: "source-wire", model: "source-model"}
	gate := permission.New(permission.ModeAsk)
	gate.AppendRules(permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"})
	loop := agent.NewLoop(sourceProvider, tools.NewRegistry(), gate, agent.NewHookRegistry(), "source-system", 2)
	loop.Model = "source-model"
	loop.SystemSections = []llm.SystemSection{{Name: "source", Body: "source-system"}}
	loop.Restore([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "source"}}}})
	sourceTimingCalled := false
	loop.TimingSink = func(string, time.Duration, bool) { sourceTimingCalled = true }

	boundaryCalls, switchCalls := 0, 0
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "source",
		ProviderName:     "source-profile",
		BuildProvider: func(string, string) (*rtpkg.ProviderBuild, error) {
			return nil, errors.New("profile is not configured")
		},
		SessionBoundary: func() { boundaryCalls++ },
		SessionSwitch:   func(string, string) { switchCalls++ },
	})
	initialSwitchCalls := switchCalls
	hdr, history, err := store.Load("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.activateSession("target", hdr, history); err == nil {
		t.Fatal("activateSession unexpectedly succeeded")
	}

	if loop.Provider != sourceProvider || loop.Model != "source-model" || loop.System != "source-system" {
		t.Fatalf("failed preflight mutated provider state: provider=%T model=%q system=%q", loop.Provider, loop.Model, loop.System)
	}
	if gate.Mode() != permission.ModeAsk || len(gate.Snapshot()) != 1 {
		t.Fatalf("failed preflight mutated permissions: mode=%q rules=%+v", gate.Mode(), gate.Snapshot())
	}
	if got := loop.History(); len(got) != 1 || got[0].Content[0].Text != "source" {
		t.Fatalf("failed preflight mutated transcript: %+v", got)
	}
	if boundaryCalls != 0 || switchCalls != initialSwitchCalls {
		t.Fatalf("failed preflight fired boundary callbacks: boundary=%d switch=%d", boundaryCalls, switchCalls)
	}
	loop.TimingSink("Read", time.Millisecond, false)
	if !sourceTimingCalled {
		t.Fatal("failed preflight replaced the source timing sink")
	}
	s.stateMu.RLock()
	activeID := s.activeSessionID
	s.stateMu.RUnlock()
	if activeID != "source" {
		t.Fatalf("active session changed to %q", activeID)
	}
}
