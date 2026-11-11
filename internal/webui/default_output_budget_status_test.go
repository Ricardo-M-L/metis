package webui

import (
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

type statusDefaultOutputBudgetProvider struct {
	webStatusCapabilitiesProvider
	budget int
}

func TestStatusWorkerDefaultOutputBudgetStaysSessionScoped(t *testing.T) {
	s, _ := testServer(t)
	for _, budget := range []int{4096, 0} {
		s.setWorkerSnapshot("worker-budget", desktopipc.Status{Context: &desktopipc.ContextPressure{
			Used: 32000, Window: 1048576, MaxOutputTokens: 128000, DefaultOutputBudgetTokens: budget,
		}})
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=worker-budget", nil))
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusOK || body["contextDefaultOutputBudgetTokens"] != float64(budget) || body["contextMaxOutputTokens"] != float64(128000) {
			t.Fatalf("worker default was lost or conflated with capacity: %#v", body)
		}
	}
}

func (p *statusDefaultOutputBudgetProvider) DefaultOutputBudget() int { return p.budget }

func TestStatusDefaultOutputBudgetIsSeparateFromModelCapacity(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	_, store := testServer(t)
	p := &statusDefaultOutputBudgetProvider{
		webStatusCapabilitiesProvider: webStatusCapabilitiesProvider{
			activationTestProvider: activationTestProvider{name: "budget-route", model: "deepseek-flash"},
			capabilities:           llm.ContextCapabilities{Window: 1048576, MaxOutputTokens: 393216, Model: "deepseek-flash", Source: "fixture"},
		},
		budget: 65536,
	}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "fixture", 2)
	loop.Model = "deepseek-flash"
	const sid = "output-budget-status"
	if err := store.WriteHeaderFull(session.Header{ID: sid, WorkDir: t.TempDir(), Provider: "budget-route", Model: "deepseek-flash", System: "fixture", Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: sid, ProviderName: "budget-route"})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || body["contextDefaultOutputBudgetTokens"] != float64(65536) || body["contextMaxOutputTokens"] != float64(393216) {
		t.Fatalf("configured request default was omitted or confused with the model capability: %#v", body)
	}
}
