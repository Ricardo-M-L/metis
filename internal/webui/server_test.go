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
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
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

func (*autoCompactPersistenceProvider) Name() string          { return "auto-compact-persistence" }
func (*autoCompactPersistenceProvider) ModelID() string       { return "auto-compact-persistence" }
func (*autoCompactPersistenceProvider) MaxContextTokens() int { return 128 }
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
		SessionSwitch:       func(id string) { switchedID = id },
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
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("turn status = %d, want 502: %s", rr.Code, rr.Body.String())
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
		SessionSwitch:   func(id string) { switchedID = id },
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
		SessionSwitch:   func(string) { switchCalls++ },
	})
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
	if boundaryCalls != 0 || switchCalls != 0 {
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
