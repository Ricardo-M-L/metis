package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type desktopResumeProvider struct {
	activationTestProvider
	calls         atomic.Int32
	secondStarted chan struct{}
	resumed       chan llm.Request
}

func (p *desktopResumeProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	switch p.calls.Add(1) {
	case 1:
		return &composerSummaryStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "write-once", ToolName: "ResumeCounter"},
			{Type: "tool_input_delta", ToolUseID: "write-once", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "write-once", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use"}, {Type: "message_stop"},
		}}, nil
	case 2:
		close(p.secondStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		p.resumed <- req
		return &composerSummaryStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "continued from saved tool result"},
			{Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"},
		}}, nil
	}
}

type desktopResumeCounterTool struct {
	tools.BaseTool
	executions *atomic.Int32
}

func (desktopResumeCounterTool) Name() string                { return "ResumeCounter" }
func (desktopResumeCounterTool) Description() string         { return "increment once to test resumed history" }
func (desktopResumeCounterTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (desktopResumeCounterTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (desktopResumeCounterTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t desktopResumeCounterTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: fmt.Sprint(t.executions.Add(1))}, nil
}

func desktopResumePost(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(body)))
	return rr
}

func desktopResumeGet(t *testing.T, s *Server, path string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("get %s = %d %s", path, rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDesktopStopResumePreservesUserAndCompletedTool(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "desktop-no-new-input"
	p := &desktopResumeProvider{activationTestProvider: activationTestProvider{name: "resume-fixture", model: "resume-model"}, secondStarted: make(chan struct{}), resumed: make(chan llm.Request, 1)}
	var effects atomic.Int32
	registry := tools.NewRegistry()
	registry.Register(desktopResumeCounterTool{executions: &effects})
	loop := agent.NewLoop(p, registry, permission.New(permission.ModeFullAccess), nil, "original system", 8)
	loop.Model = "resume-model"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "resume-fixture", Model: "resume-model", System: "original system", Mode: string(permission.ModeFullAccess), WorkDir: t.TempDir(), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "resume-fixture", InitialSessionID: id})
	turnDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { turnDone <- desktopResumePost(t, s, `{"sessionId":"`+id+`","input":"original request"}`) }()
	select {
	case <-p.secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("tool round did not finish")
	}
	stop := httptest.NewRecorder()
	s.handler().ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "/api/stop", strings.NewReader(`{"sessionId":"`+id+`"}`)))
	if stop.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", stop.Code, stop.Body.String())
	}
	select {
	case rr := <-turnDone:
		if rr.Code != http.StatusOK {
			t.Fatalf("turn = %d %s", rr.Code, rr.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not settle")
	}
	status := desktopResumeGet(t, s, "/api/status?sessionId="+id)
	if status["resumeAvailable"] != true || status["resumeSessionId"] != id {
		t.Fatalf("stop snapshot = %+v", status)
	}
	token, _ := status["resumeToken"].(string)
	if token == "" {
		t.Fatal("stop omitted resume token")
	}
	resumed := desktopResumePost(t, s, `{"sessionId":"`+id+`","resume":true,"resumeToken":"`+token+`"}`)
	if resumed.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", resumed.Code, resumed.Body.String())
	}
	req := <-p.resumed
	userCount, results := 0, 0
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if msg.Role == llm.RoleUser && block.Type == "text" && !block.Synthetic {
				userCount++
			}
			if block.Type == "tool_result" && block.ToolUseID == "write-once" && block.ToolResult == "1" {
				results++
			}
		}
	}
	if userCount != 1 || results != 1 || effects.Load() != 1 {
		t.Fatalf("resume history/effects: users=%d results=%d effects=%d %+v", userCount, results, effects.Load(), req.Messages)
	}
	if !strings.Contains(req.System, "未确认完成的工具可能已产生外部副作用") {
		t.Fatal("resume omitted interrupted side-effect reminder")
	}
	_, saved, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, msg := range saved {
		if msg.Role == llm.RoleUser {
			for _, block := range msg.Content {
				if block.Type == "text" && !block.Synthetic {
					users++
				}
			}
		}
	}
	if users != 1 {
		t.Fatalf("resume appended user input: %+v", saved)
	}
	if got := desktopResumePost(t, s, `{"sessionId":"`+id+`","resume":true,"resumeToken":"`+token+`"}`); got.Code != http.StatusConflict {
		t.Fatalf("reused token = %d %s", got.Code, got.Body.String())
	}
}

type desktopResumeIsolatedFixture struct {
	store        *session.Store
	started      chan IsolatedTurnRequest
	release      chan struct{}
	calls        atomic.Int32
	checkpointed bool
}

func (*desktopResumeIsolatedFixture) Eligible(*session.Header) bool { return true }
func (f *desktopResumeIsolatedFixture) Run(ctx context.Context, request IsolatedTurnRequest) (IsolatedTurnResult, error) {
	f.calls.Add(1)
	if !request.Resume {
		if err := f.store.AppendMessage(request.SessionID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: request.Input}}}); err != nil {
			return IsolatedTurnResult{}, err
		}
	}
	f.started <- request
	select {
	case <-ctx.Done():
		return IsolatedTurnResult{Stopped: true, Checkpointed: f.checkpointed}, ctx.Err()
	case <-f.release:
		return IsolatedTurnResult{Checkpointed: f.checkpointed}, nil
	}
}

func desktopResumeIsolatedServer(t *testing.T, store *session.Store, fixture *desktopResumeIsolatedFixture) *Server {
	t.Helper()
	return NewServer("127.0.0.1:0", agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 4), store, RuntimeBindings{IsolatedRunner: fixture, IsolatedTurns: &IsolatedTurnOptions{Executable: "test", MaxParallel: 2}})
}

func desktopResumeSeed(t *testing.T, store *session.Store, id string) string {
	t.Helper()
	if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: t.TempDir(), Mode: string(permission.ModeFullAccess), Model: "fixture", Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "original " + id}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishDesktopTurn(id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	hdr, _, _ := store.LoadHeader(id)
	return hdr.ResumeToken
}

func TestDesktopResumeTokenAdmissionAcrossDesktopInstances(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := desktopResumeSeed(t, store, "same-session")
	other, err := session.NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &desktopResumeIsolatedFixture{store: store, started: make(chan IsolatedTurnRequest, 2), release: make(chan struct{}), checkpointed: true}
	a, b := desktopResumeIsolatedServer(t, store, f), desktopResumeIsolatedServer(t, other, f)
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, s := range []*Server{a, b} {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			responses <- desktopResumePost(t, s, `{"sessionId":"same-session","resume":true,"resumeToken":"`+token+`"}`)
		}(s)
	}
	select {
	case req := <-f.started:
		if !req.Resume || req.Input != "" {
			t.Fatalf("resume worker input = %+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not start")
	}
	close(f.release)
	wg.Wait()
	close(responses)
	statuses := map[int]int{}
	for rr := range responses {
		statuses[rr.Code]++
	}
	if f.calls.Load() != 1 || statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("duplicate admission: calls=%d statuses=%v", f.calls.Load(), statuses)
	}
}

func TestDesktopResumeMetadataOwnershipRestartAndGuards(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokenA := desktopResumeSeed(t, store, "session-a")
	tokenB := desktopResumeSeed(t, store, "session-b")
	f := &desktopResumeIsolatedFixture{store: store, started: make(chan IsolatedTurnRequest, 3), release: make(chan struct{}), checkpointed: true}
	close(f.release)
	s := desktopResumeIsolatedServer(t, store, f)
	for _, path := range []string{"/api/status?sessionId=session-b", "/api/sessions/session-b"} {
		snap := desktopResumeGet(t, s, path)
		if snap["resumeSessionId"] != "session-b" || snap["resumeToken"] != tokenB || snap["resumeAvailable"] != true {
			t.Fatalf("B snapshot = %+v", snap)
		}
	}
	if rr := desktopResumePost(t, s, `{"sessionId":"session-b","resume":true,"resumeToken":"`+tokenA+`"}`); rr.Code != http.StatusConflict {
		t.Fatalf("A token resumed B: %d", rr.Code)
	}
	for _, body := range []string{
		`{"resume":true,"resumeToken":"` + tokenA + `"}`,
		`{"sessionId":"session-a","resume":true,"resumeToken":"` + tokenA + `","input":""}`,
		`{"sessionId":"session-a","resume":true,"resumeToken":"` + tokenA + `","images":[]}`,
		`{"sessionId":"session-a","resumeToken":"` + tokenA + `","input":"new"}`,
	} {
		if rr := desktopResumePost(t, s, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("guard %s = %d %s", body, rr.Code, rr.Body.String())
		}
	}
	if rr := desktopResumePost(t, s, `{"sessionId":"session-a","input":"new ordinary input"}`); rr.Code != http.StatusOK {
		t.Fatalf("ordinary turn = %d %s", rr.Code, rr.Body.String())
	}
	if rr := desktopResumePost(t, s, `{"sessionId":"session-a","resume":true,"resumeToken":"`+tokenA+`"}`); rr.Code != http.StatusConflict {
		t.Fatalf("new input retained old token: %d", rr.Code)
	}
	other, err := session.NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := desktopResumeIsolatedServer(t, other, f)
	if rr := desktopResumePost(t, restarted, `{"sessionId":"session-b","resume":true,"resumeToken":"`+tokenB+`"}`); rr.Code != http.StatusOK {
		t.Fatalf("restart resume = %d %s", rr.Code, rr.Body.String())
	}
}

func TestDesktopStopWithoutCheckpointDoesNotOfferResume(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	desktopResumeSeed(t, store, "unclean")
	f := &desktopResumeIsolatedFixture{store: store, started: make(chan IsolatedTurnRequest, 1), release: make(chan struct{}), checkpointed: false}
	s := desktopResumeIsolatedServer(t, store, f)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- desktopResumePost(t, s, `{"sessionId":"unclean","input":"run"}`) }()
	select {
	case <-f.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	stop := httptest.NewRecorder()
	s.handler().ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "/api/stop", strings.NewReader(`{"sessionId":"unclean"}`)))
	<-done
	snap := desktopResumeGet(t, s, "/api/status?sessionId=unclean")
	if snap["resumeAvailable"] != false || snap["resumeToken"] != "" || snap["resumeSessionId"] != "unclean" {
		t.Fatalf("unclean stop snapshot = %+v", snap)
	}
}

func TestDesktopResumeForkStartsFreshTurnLifetime(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const parent = "stopped-parent"
	token := desktopResumeSeed(t, store, parent)
	f := &desktopResumeIsolatedFixture{store: store, started: make(chan IsolatedTurnRequest, 1), release: make(chan struct{}), checkpointed: true}
	close(f.release)
	s := desktopResumeIsolatedServer(t, store, f)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/fork", strings.NewReader(`{"sessionId":"`+parent+`","messageIndex":-1}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("fork = %d %s", rr.Code, rr.Body.String())
	}
	var response struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	child, _, err := store.LoadHeader(response.SessionID)
	if err != nil || child.Status != "idle" || child.ResumeToken != "" || child.ClearResumeToken {
		t.Fatalf("branch inherited stopped checkpoint: %+v, %v", child, err)
	}
	for _, path := range []string{"/api/sessions/" + response.SessionID, "/api/status?sessionId=" + response.SessionID} {
		snapshot := desktopResumeGet(t, s, path)
		if snapshot["resumeAvailable"] != false || snapshot["resumeToken"] != "" || snapshot["resumeSessionId"] != response.SessionID {
			t.Fatalf("branch offers parent continuation: %+v", snapshot)
		}
	}
	if rr := desktopResumePost(t, s, `{"sessionId":"`+response.SessionID+`","resume":true,"resumeToken":"`+token+`"}`); rr.Code != http.StatusConflict {
		t.Fatalf("parent token resumed branch: %d %s", rr.Code, rr.Body.String())
	}
	if snapshot := desktopResumeGet(t, s, "/api/sessions/"+parent); snapshot["resumeAvailable"] != true || snapshot["resumeToken"] != token {
		t.Fatalf("fork invalidated parent token: %+v", snapshot)
	}
	if rr := desktopResumePost(t, s, `{"sessionId":"`+parent+`","resume":true,"resumeToken":"`+token+`"}`); rr.Code != http.StatusOK {
		t.Fatalf("parent token no longer valid: %d %s", rr.Code, rr.Body.String())
	}
}

func TestDesktopResumeHistoryEditsInvalidateOnlyChangedCheckpoint(t *testing.T) {
	for _, command := range []string{"undo", "retry", "clear-history", "compact", "save", "undo-noop", "compact-noop"} {
		t.Run(command, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "stopped-history-edit"
			history := seedComposerHistory(t, store, id, t.TempDir())
			if strings.HasSuffix(command, "-noop") {
				history = []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "no user turn to undo"}}}}
				cursor := session.NewHistoryCursor(nil)
				if err := store.ReplaceHistoryAndMark(id, history, &cursor); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.FinishDesktopTurn(id, "stopped", true); err != nil {
				t.Fatal(err)
			}
			hdr, _, err := store.LoadHeader(id)
			if err != nil {
				t.Fatal(err)
			}
			token := hdr.ResumeToken
			provider := &composerSummaryProvider{}
			loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 4)
			config := agent.DefaultCompactionConfig()
			config.ProtectFirst = 1
			config.ProtectLast = 1
			loop.Compactor = agent.NewCompactor(config, provider.ModelID(), provider.MaxContextTokens(), provider)
			loop.Restore(history)
			s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: id, ProviderName: "wire"})
			path, requestedCommand := "/api/commands/session", strings.TrimSuffix(command, "-noop")
			if requestedCommand == "compact" {
				path = "/api/compact"
			}
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"sessionId":"`+id+`","command":"`+requestedCommand+`"}`)))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s = %d %s", command, rr.Code, rr.Body.String())
			}
			changed := command != "save" && !strings.HasSuffix(command, "-noop")
			snapshot := desktopResumeGet(t, s, "/api/sessions/"+id)
			if snapshot["resumeAvailable"] != !changed {
				t.Fatalf("%s changed=%t snapshot=%+v", command, changed, snapshot)
			}
			if changed {
				if snapshot["resumeToken"] != "" {
					t.Fatalf("%s retained token: %+v", command, snapshot)
				}
				if rr := desktopResumePost(t, s, `{"sessionId":"`+id+`","resume":true,"resumeToken":"`+token+`"}`); rr.Code != http.StatusConflict {
					t.Fatalf("%s accepted old checkpoint token: %d %s", command, rr.Code, rr.Body.String())
				}
			} else if snapshot["resumeToken"] != token {
				t.Fatalf("%s invalidated an unchanged checkpoint", command)
			}
		})
	}
}

func TestDesktopResumeHistoryEditsRejectOtherDesktopSessionOwner(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "remote-running-edit"
	history := seedComposerHistory(t, store, id, t.TempDir())
	if err := store.FinishDesktopTurn(id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	header, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatal(err)
	}
	token := header.ResumeToken
	owner, err := store.AcquireTurnLock(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	other, err := session.NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	provider := &composerSummaryProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 4)
	loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), provider.ModelID(), provider.MaxContextTokens(), provider)
	s := NewServer("127.0.0.1:0", loop, other, RuntimeBindings{ProviderName: "wire"})
	for _, command := range []string{"undo", "retry", "clear-history", "compact", "save"} {
		path := "/api/commands/session"
		if command == "compact" {
			path = "/api/compact"
		}
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"sessionId":"`+id+`","command":"`+command+`"}`)))
		if rr.Code != http.StatusConflict {
			t.Fatalf("cross-Desktop %s = %d %s", command, rr.Code, rr.Body.String())
		}
	}
	hdr, after, err := store.Load(id)
	if err != nil || hdr.ResumeToken != token || len(after) != len(history) {
		t.Fatalf("busy edit changed checkpoint: %+v %+v %v", hdr, after, err)
	}
}
