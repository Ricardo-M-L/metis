package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
