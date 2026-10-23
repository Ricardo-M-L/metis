package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type webStatusCapabilitiesProvider struct {
	activationTestProvider
	capabilities llm.ContextCapabilities
}

func (p *webStatusCapabilitiesProvider) ContextCapabilities() llm.ContextCapabilities {
	return p.capabilities
}
func (*webStatusCapabilitiesProvider) RefreshContextCapabilities(context.Context) error {
	panic("status must not refresh endpoint metadata")
}

func TestStatusMainLoopUsesOneCachedCapabilityBinding(t *testing.T) {
	s, store := testServer(t)
	p := &webStatusCapabilitiesProvider{activationTestProvider: activationTestProvider{name: "custom-route", model: "new-model"}, capabilities: llm.ContextCapabilities{Window: 200000, Source: "fallback"}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "preserved system", 2)
	loop.Model = p.model
	loop.AppendUser("parent-owned transcript")
	if err := store.WriteHeaderFull(session.Header{ID: "parent-session", WorkDir: t.TempDir(), Provider: p.name, Model: p.model, System: "preserved system", Mode: string(permission.ModeAsk), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s = NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "parent-session", ProviderName: p.name})
	p.capabilities = llm.ContextCapabilities{Window: 272000, DefaultWindow: 272000, MaxWindow: 872000, MaxInputTokens: 144000, MaxOutputTokens: 128000, Source: "endpoint_pi"}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=parent-session", nil))
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || payload["contextSessionId"] != "parent-session" || payload["contextWindow"] != float64(272000) || payload["contextDefaultWindow"] != float64(272000) || payload["contextMaxWindow"] != float64(872000) || payload["contextMaxInputTokens"] != float64(144000) || payload["contextMaxOutputTokens"] != float64(128000) || payload["contextSource"] != "endpoint_pi" {
		t.Fatalf("parent metadata missing, stale, or conflated: %#v", payload)
	}
	p.capabilities = llm.ContextCapabilities{Window: 1050000, DefaultWindow: 272000, ProfileDefaultWindow: 272000, ProfileMaxWindow: 872000, MaxWindow: 1050000, MaxInputTokens: 922000, MaxOutputTokens: 128000, Source: "catalog_profile"}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=parent-session", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || payload["contextSessionId"] != "parent-session" || payload["contextWindow"] != float64(1050000) || payload["contextDefaultWindow"] != float64(272000) || payload["contextMaxWindow"] != float64(1050000) || payload["contextProfileDefaultWindow"] != float64(272000) || payload["contextProfileMaxWindow"] != float64(872000) || payload["contextMaxInputTokens"] != float64(922000) || payload["contextSource"] != "catalog_profile" {
		t.Fatalf("full model context was replaced by client profile: %#v", payload)
	}
}

func TestStatusWorkerKeepsFullCapacitySeparateFromClientProfile(t *testing.T) {
	s, _ := testServer(t)
	s.setWorkerSnapshot("profile-session", desktopipc.Status{Context: &desktopipc.ContextPressure{
		Used: 30488, Window: 1050000, DefaultWindow: 272000, ProfileDefaultWindow: 272000, ProfileMaxWindow: 872000,
		MaxWindow: 1050000, MaxInputTokens: 922000, MaxOutputTokens: 128000, Source: "catalog_profile", CompactThreshold: 0.8, CompactAtTokens: 737600,
	}})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=profile-session", nil))
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextSessionId"] != "profile-session" || payload["contextWindow"] != float64(1050000) || payload["contextMaxWindow"] != float64(1050000) || payload["contextProfileDefaultWindow"] != float64(272000) || payload["contextProfileMaxWindow"] != float64(872000) || payload["compactAtTokens"] != float64(737600) {
		t.Fatalf("worker profile metadata lost: %#v", payload)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=other-session", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextProfileDefaultWindow"] != float64(0) || payload["contextProfileMaxWindow"] != float64(0) {
		t.Fatalf("client profile leaked into unrelated session: %#v", payload)
	}
}

func TestStatusIPCModelBudgetMetadataSurvivesAndStaysSessionScoped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pressure desktopipc.ContextPressure
		want     map[string]any
	}{
		{
			name: "complete model and recommendation independent of clamped working budget",
			pressure: desktopipc.ContextPressure{Used: 30488, Window: 128000, WindowKind: "total",
				DefaultWindow: 128000, MaxWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 32000,
				ProfileDefaultWindow: 500000, ProfileMaxWindow: 872000, Source: "configured_clamped",
				ModelWindow: 1050000, ModelMaxInputTokens: 922000, ModelMaxOutputTokens: 128000, ModelSource: "catalog_model_spec",
				RequestedWindow: 800000, LimitSource: "endpoint_models", BudgetSource: "per_model",
				ProviderID: "worker-route", Model: "exact-model", RouteKey: "account-scoped-route",
				CompactThreshold: 0.8, CompactAtTokens: 76800},
			want: map[string]any{
				"contextWindow": float64(128000), "contextWindowKind": "total", "contextDefaultWindow": float64(128000),
				"contextMaxWindow": float64(128000), "contextMaxInputTokens": float64(96000), "contextMaxOutputTokens": float64(32000),
				"contextProfileDefaultWindow": float64(500000), "contextProfileMaxWindow": float64(872000), "contextSource": "configured_clamped",
				"contextModelWindow": float64(1050000), "contextModelMaxInputTokens": float64(922000), "contextModelMaxOutputTokens": float64(128000),
				"contextModelSource": "catalog_model_spec", "contextRequestedWindow": float64(800000), "contextLimitSource": "endpoint_models",
				"contextBudgetSource": "per_model", "contextProviderId": "worker-route", "contextModel": "exact-model", "contextRouteKey": "account-scoped-route",
				"compactAtTokens": float64(76800),
			},
		},
		{
			name: "input ceiling preserves unknown total",
			pressure: desktopipc.ContextPressure{Used: 30488, Window: 160000, WindowKind: "input", MaxInputTokens: 160000, MaxOutputTokens: 64000,
				Source: "endpoint_input_limit", BudgetSource: "automatic", ProviderID: "input-route", Model: "input-model", RouteKey: "input-route-key"},
			want: map[string]any{
				"contextWindow": float64(160000), "contextWindowKind": "input", "contextMaxWindow": float64(0), "contextModelWindow": float64(0),
				"contextMaxInputTokens": float64(160000), "contextMaxOutputTokens": float64(64000), "contextProfileMaxWindow": float64(0),
				"contextSource": "endpoint_input_limit", "contextBudgetSource": "automatic", "contextProviderId": "input-route",
				"contextModel": "input-model", "contextRouteKey": "input-route-key",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t)
			var wire bytes.Buffer
			if err := desktopipc.NewEncoder(&wire).Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeStatus, Status: &desktopipc.Status{Context: &tc.pressure}}); err != nil {
				t.Fatal(err)
			}
			decoded, err := desktopipc.NewDecoder(&wire).Decode()
			if err != nil || decoded.Status == nil {
				t.Fatalf("decode worker status: %v", err)
			}
			s.setWorkerSnapshot("owned-model-session", *decoded.Status)
			read := func(sessionID string) map[string]any {
				t.Helper()
				rr := httptest.NewRecorder()
				s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sessionID, nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("status HTTP=%d body=%s", rr.Code, rr.Body.String())
				}
				var payload map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				return payload
			}
			payload := read("owned-model-session")
			if payload["contextSessionId"] != "owned-model-session" || payload["contextUsed"] != float64(30488) {
				t.Fatalf("worker context ownership missing: %#v", payload)
			}
			for key, want := range tc.want {
				if payload[key] != want {
					t.Fatalf("IPC to HTTP %s=%v; want %v", key, payload[key], want)
				}
			}
			unowned := read("unrelated-session")
			for _, key := range []string{"contextModelWindow", "contextModelMaxInputTokens", "contextModelMaxOutputTokens", "contextRequestedWindow"} {
				if unowned[key] != float64(0) {
					t.Fatalf("worker numeric metadata leaked to another session: %s=%v", key, unowned[key])
				}
			}
			for _, key := range []string{"contextModelSource", "contextLimitSource", "contextBudgetSource", "contextProviderId", "contextModel", "contextRouteKey", "contextWindowKind"} {
				if unowned[key] != "" {
					t.Fatalf("worker metadata owner leaked to another session: %s=%v", key, unowned[key])
				}
			}
		})
	}
}

func TestStatusMainLoopPreservesNewBudgetMetadataWithoutDiscovery(t *testing.T) {
	_, store := testServer(t)
	p := &webStatusCapabilitiesProvider{activationTestProvider: activationTestProvider{name: "parent-route", model: "parent-model"},
		capabilities: llm.ContextCapabilities{Window: 800000, WindowKind: "total", ModelWindow: 1050000,
			ModelMaxInputTokens: 922000, ModelMaxOutputTokens: 128000, ModelSource: "catalog_model_spec",
			ProfileDefaultWindow: 500000, ProfileMaxWindow: 872000, Source: "configured", RequestedWindow: 800000,
			BudgetSource: "per_model", ProviderID: "parent-route", Model: "parent-model", RouteKey: "parent-account-key"}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = p.model
	loop.AppendUser("parent owned prompt")
	if err := store.WriteHeaderFull(session.Header{ID: "parent-budget-session", WorkDir: t.TempDir(), Provider: p.name, Model: p.model, System: "system", Mode: string(permission.ModeAsk), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "parent-budget-session", ProviderName: p.name})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=parent-budget-session", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"contextSessionId": "parent-budget-session", "contextWindow": float64(800000), "contextWindowKind": "total", "contextDefaultWindow": float64(0),
		"contextModelWindow": float64(1050000), "contextModelMaxInputTokens": float64(922000), "contextModelMaxOutputTokens": float64(128000), "contextModelSource": "catalog_model_spec",
		"contextProfileDefaultWindow": float64(500000), "contextProfileMaxWindow": float64(872000), "contextRequestedWindow": float64(800000), "contextBudgetSource": "per_model",
		"contextProviderId": "parent-route", "contextModel": "parent-model", "contextRouteKey": "parent-account-key",
	} {
		if payload[key] != want {
			t.Fatalf("parent Loop to HTTP %s=%v; want %v", key, payload[key], want)
		}
	}
}

func TestStatusUnknownViewedWorkerWinsOverPositiveParentCapacity(t *testing.T) {
	_, store := testServer(t)
	p := &webStatusCapabilitiesProvider{activationTestProvider: activationTestProvider{name: "parent-route", model: "parent-model"},
		capabilities: llm.ContextCapabilities{Window: 1000000, WindowKind: "total", ModelWindow: 1000000, Source: "endpoint_models",
			BudgetSource: "automatic", ProviderID: "parent-route", Model: "parent-model", RouteKey: "parent-account-key"}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = p.model
	loop.AppendUser("positive parent context unrelated to viewed worker")
	if err := store.WriteHeaderFull(session.Header{ID: "positive-parent-session", WorkDir: t.TempDir(), Provider: p.name, Model: p.model, System: "system", Mode: string(permission.ModeAsk), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "positive-parent-session", ProviderName: p.name})
	s.setWorkerSnapshot("unknown-worker-session", desktopipc.Status{Context: &desktopipc.ContextPressure{
		Used: 31415, Window: 0, WindowKind: "total", Source: "unknown", BudgetSource: "automatic",
		ProviderID: "unknown-worker-route", Model: "unknown-worker-model", RouteKey: "worker-account-key",
	}})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=unknown-worker-session", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"contextSessionId": "unknown-worker-session", "contextUsed": float64(31415), "contextWindow": float64(0), "contextModelWindow": float64(0),
		"contextSource": "unknown", "contextBudgetSource": "automatic", "contextRequestedWindow": float64(0),
		"contextProviderId": "unknown-worker-route", "contextModel": "unknown-worker-model", "contextRouteKey": "worker-account-key",
	} {
		if payload[key] != want {
			t.Fatalf("unknown viewed worker borrowed another Loop's capacity: %s=%v want=%v", key, payload[key], want)
		}
	}
}

func TestStatusDropsOldWorkerContextAfterSameSessionActivation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, loop, _, _, sid := modelSwitchFixture(t, t.TempDir())
	s.setWorkerSnapshot(sid, desktopipc.Status{Context: &desktopipc.ContextPressure{Used: 48_000, Window: 128_000}})
	if err := s.store.AppendMessage(sid, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new image turn"}}}); err != nil {
		t.Fatal(err)
	}
	header, history, err := s.store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	// Image turns run on the parent Loop after activating their transcript.
	if err := s.activateSession(sid, header, history); err != nil {
		t.Fatal(err)
	}
	loop.AppendUser("new in-process prompt")
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextSessionId"] != sid || payload["contextUsed"] != float64(loop.EstimateContextTokens()) {
		t.Fatalf("old worker reading obscured the current parent Loop: %#v", payload)
	}
}

func TestModelSwitchInvalidatesOnlySelectedWorkerContext(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _, _, _, sid := modelSwitchFixture(t, t.TempDir())
	old := desktopipc.Status{Context: &desktopipc.ContextPressure{Used: 48_000, Window: 128_000}, Agents: []desktopipc.Subagent{{AgentID: "keep-agent", Status: "completed"}}}
	s.setWorkerSnapshot(sid, old)
	s.setWorkerSnapshot("other-session", old)
	// A completed isolated turn leaves the parent's history unowned/stale.
	s.contextMu.Lock()
	s.contextSessionID = ""
	s.contextMu.Unlock()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/models", bytes.NewBufferString(`{"provider":"anthropic","model":"claude-new"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("model switch %d: %s", rr.Code, rr.Body.String())
	}
	selected, _ := s.workerSnapshot(sid)
	other, _ := s.workerSnapshot("other-session")
	if selected.Context != nil || len(selected.Agents) != 1 || other.Context == nil {
		t.Fatalf("incorrect context invalidation: selected=%+v other=%+v", selected, other)
	}
	status := httptest.NewRecorder()
	s.handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	var payload map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextSessionId"] != nil {
		t.Fatalf("model switch kept obsolete worker context: %#v", payload)
	}
}

func TestStatusUsesViewedWorkerContextAndRetainsFinalSnapshot(t *testing.T) {
	s, _ := testServer(t)
	s.stateMu.Lock()
	s.activeSessionID = "session-a"
	s.stateMu.Unlock()
	s.setWorkerSnapshot("session-b", desktopipc.Status{Context: &desktopipc.ContextPressure{
		Used: 48_000, Window: 128_000, CompactThreshold: 0.8, CompactAtTokens: 100_000,
		DefaultWindow: 128_000, MaxWindow: 1_000_000, MaxInputTokens: 96_000, MaxOutputTokens: 32_000, Source: "endpoint_pi",
	}})
	read := func(path string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	viewB := read("/api/status?sessionId=session-b")
	if viewB["contextSessionId"] != "session-b" || viewB["contextUsed"] != float64(48_000) ||
		viewB["contextWindow"] != float64(128_000) || viewB["compactAtTokens"] != float64(100_000) ||
		viewB["contextDefaultWindow"] != float64(128_000) || viewB["contextMaxWindow"] != float64(1_000_000) ||
		viewB["contextMaxInputTokens"] != float64(96_000) || viewB["contextMaxOutputTokens"] != float64(32_000) || viewB["contextSource"] != "endpoint_pi" {
		t.Fatalf("viewed worker pressure missing: %#v", viewB)
	}
	viewA := read("/api/status?sessionId=session-a")
	if viewA["contextSessionId"] == "session-b" || viewA["contextSource"] == "endpoint_pi" {
		t.Fatalf("worker B pressure leaked into A: %#v", viewA)
	}
	s.stateMu.Lock()
	s.activeSessionID = "session-b"
	s.stateMu.Unlock()
	activeB := read("/api/status")
	if activeB["contextSessionId"] != "session-b" || activeB["contextUsed"] != float64(48_000) {
		t.Fatalf("active worker pressure missing without a view query: %#v", activeB)
	}
	s.finishWorkerSnapshot("session-b", false, false)
	finalB := read("/api/status?sessionId=session-b")
	if finalB["contextSessionId"] != "session-b" || finalB["contextUsed"] != float64(48_000) {
		t.Fatalf("completed worker pressure was lost: %#v", finalB)
	}
}

func TestStatusContextBreakdownIsOptionalValidatedAndSessionScoped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		parts   *desktopipc.ContextBreakdown
		present bool
	}{
		{"old worker", nil, false},
		{"zero is valid", &desktopipc.ContextBreakdown{Estimated: true}, true},
		{"estimated categories", &desktopipc.ContextBreakdown{SystemTokens: 1400, ToolTokens: 7100, MessageTokens: 239000, Estimated: true}, true},
		{"negative category", &desktopipc.ContextBreakdown{MessageTokens: -1, Estimated: true}, false},
		{"missing estimate identity", &desktopipc.ContextBreakdown{MessageTokens: 10}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t)
			s.setWorkerSnapshot("breakdown-owner", desktopipc.Status{Context: &desktopipc.ContextPressure{Used: 331000, Window: 1000000, Model: "exact-model", Breakdown: tc.parts}})
			request := func(sid string) map[string]any {
				rr := httptest.NewRecorder()
				s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("status %d", rr.Code)
				}
				var payload map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				return payload
			}
			got := request("breakdown-owner")
			_, present := got["contextBreakdown"]
			if present != tc.present || got["contextUsed"] != float64(331000) || got["contextWindow"] != float64(1000000) {
				t.Fatalf("wrong category contract: %#v", got)
			}
			if tc.present {
				parts := got["contextBreakdown"].(map[string]any)
				if parts["messageTokens"] != float64(tc.parts.MessageTokens) || parts["estimated"] != true {
					t.Fatalf("normalized or malformed categories: %#v", parts)
				}
			}
			other := request("other-session")
			if _, leaked := other["contextBreakdown"]; leaked {
				t.Fatalf("worker categories leaked across session: %#v", other)
			}
		})
	}
}

func TestStatusMainLoopPublishesOnlyPreparedContextBreakdown(t *testing.T) {
	_, store := testServer(t)
	p := &webStatusCapabilitiesProvider{activationTestProvider: activationTestProvider{name: "route", model: "model"}, capabilities: llm.ContextCapabilities{Window: 1000000, Model: "model"}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system body", 2)
	loop.Model = p.model
	loop.AppendUser("owned messages")
	loop.SyncContextCapabilities()
	loop.EstimateRequestContextTokens(nil)
	if err := store.WriteHeaderFull(session.Header{ID: "prepared-owner", WorkDir: t.TempDir(), Provider: p.name, Model: p.model, System: "system body", Mode: string(permission.ModeAsk), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "prepared-owner"})
	loop.EstimateRequestContextTokens(nil)
	// A status read must consume the prepared prefix instead of assembling fresh
	// runtime state. This callback would expose any accidental request rebuild.
	loop.CurrentStateSections = func() []llm.SystemSection { panic("status assembled runtime state") }
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["contextSessionId"] != "prepared-owner" || got["contextBreakdown"] == nil {
		t.Fatalf("prepared local breakdown missing: %#v", got)
	}
	loop.ResetSession(nil)
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	got = make(map[string]any)
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["contextBreakdown"]; present {
		t.Fatalf("replaced history retained old prefix: %#v", got)
	}
}
