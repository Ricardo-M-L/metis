package webui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	"github.com/Ricardo-M-L/metis/internal/version"
)

func TestDesktopConfigReportsTheRealMetisVersion(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"version":"`+version.Short()+`"`)) {
		t.Fatalf("config version = %d: %s, want %s", rr.Code, rr.Body.String(), version.Short())
	}
}

type composerSummaryProvider struct{}

func (*composerSummaryProvider) Name() string          { return "composer-summary" }
func (*composerSummaryProvider) ModelID() string       { return "composer-summary" }
func (*composerSummaryProvider) MaxContextTokens() int { return 200_000 }
func (*composerSummaryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("manual compaction must use streaming")
}
func (*composerSummaryProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &composerSummaryStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "COMPOSER_SUMMARY"},
		{Type: "message_stop"},
	}}, nil
}

type composerSummaryStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *composerSummaryStream) Close() error { return nil }
func (s *composerSummaryStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func TestComposerGoalAPIListsAndCreatesRealGoals(t *testing.T) {
	store := builtin.NewGoalStore(filepath.Join(t.TempDir(), "goals"))
	builtin.SetGoalStore(store)
	t.Cleanup(func() { builtin.SetGoalStore(nil) })

	s, _ := testServer(t)
	h := s.handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/goals", bytes.NewBufferString(`{"objective":"Ship the Desktop command menu","priority":"high"}`)))
	if rr.Code != http.StatusCreated || !bytes.Contains(rr.Body.Bytes(), []byte("Ship the Desktop command menu")) {
		t.Fatalf("create goal = %d: %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/goals", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"priority":"high"`)) {
		t.Fatalf("list goals = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPermissionSettingKeepsPlanGateAndLoopInSync(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	gate := permission.New(permission.ModeAcceptEdits)
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), gate, nil, "system", 2)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store)
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"changes":[{"key":"permission.mode","value":"plan"}]}`)))
	if rr.Code != http.StatusOK || gate.Mode() != permission.ModePlan || !loop.IsPlanMode() {
		t.Fatalf("enter plan = %d gate=%q loop=%v body=%s", rr.Code, gate.Mode(), loop.IsPlanMode(), rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"changes":[{"key":"permission.mode","value":"acceptEdits"}]}`)))
	if rr.Code != http.StatusOK || gate.Mode() != permission.ModeAcceptEdits || loop.IsPlanMode() {
		t.Fatalf("leave plan = %d gate=%q loop=%v body=%s", rr.Code, gate.Mode(), loop.IsPlanMode(), rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"changes":[{"key":"ui.thinking_display","value":"show"}]}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"ui.thinking_display"`)) {
		t.Fatalf("thinking display = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDesktopSessionCommandsPersistUndoClearAndSave(t *testing.T) {
	provider := &composerSummaryProvider{}
	gate := permission.New(permission.ModeBypassPermissions)
	loop := agent.NewLoop(provider, tools.NewRegistry(), gate, nil, "system", 2)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "composer-session-command"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: provider.ModelID(), System: "system"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first prompt"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "first answer"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "retry me"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "second answer"}}},
	} {
		if err := store.AppendMessage(id, message); err != nil {
			t.Fatal(err)
		}
	}
	_, history, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	loop.Restore(history)
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: id, ProviderName: "wire"})
	h := s.handler()

	post := func(command string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/commands/session", bytes.NewBufferString(`{"sessionId":"`+id+`","command":"`+command+`"}`)))
		return rr
	}

	rr := post("save")
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"saved":true`)) {
		t.Fatalf("save = %d: %s", rr.Code, rr.Body.String())
	}
	rr = post("undo")
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"prefill":"retry me"`)) {
		t.Fatalf("undo = %d: %s", rr.Code, rr.Body.String())
	}
	_, durable, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(durable) != 2 {
		t.Fatalf("undo durable history = %d messages, want 2", len(durable))
	}
	rr = post("clear-history")
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"cleared":true`)) {
		t.Fatalf("clear-history = %d: %s", rr.Code, rr.Body.String())
	}
	_, durable, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(durable) != 0 || len(loop.History()) != 0 {
		t.Fatalf("clear-history left durable=%d live=%d", len(durable), len(loop.History()))
	}
}

func TestManualCompactEndpointReplacesDurableHistory(t *testing.T) {
	provider := &composerSummaryProvider{}
	gate := permission.New(permission.ModeBypassPermissions)
	loop := agent.NewLoop(provider, tools.NewRegistry(), gate, nil, "system", 2)
	compactConfig := agent.DefaultCompactionConfig()
	compactConfig.ProtectFirst = 1
	compactConfig.ProtectLast = 1
	loop.Compactor = agent.NewCompactor(compactConfig, provider.ModelID(), provider.MaxContextTokens(), provider)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "composer-compact"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "wire", Model: provider.ModelID(), System: "system"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		message := llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("history ", 200)}}}
		if err := store.AppendMessage(id, message); err != nil {
			t.Fatal(err)
		}
	}
	hdr, history, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	loop.Restore(history)
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: id, ProviderName: "wire"})
	s.stateMu.Lock()
	s.activeSessionID = id
	s.activeProviderName = "wire"
	s.activeModel = provider.ModelID()
	s.stateMu.Unlock()
	_ = hdr

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/compact", bytes.NewBufferString(`{"sessionId":"`+id+`"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"compacted":true`)) {
		t.Fatalf("compact = %d: %s", rr.Code, rr.Body.String())
	}
	_, durable, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(durable) >= len(history) {
		t.Fatalf("durable history did not shrink: before=%d after=%d", len(history), len(durable))
	}
}
