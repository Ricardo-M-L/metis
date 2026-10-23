package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func modelEffortSettingsFixture(t *testing.T, enabled bool, values map[string]string) (*Server, *agent.Loop) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	snapshot, err := config.ReadProviderSettings()
	if err != nil {
		t.Fatal(err)
	}
	profile := config.ProviderSettingsProvider{
		ID: "private-route", Transport: "openai_chat", BaseURL: "https://private.example/v1", Model: "private-model-9287",
		Models: []config.ProviderModelSettings{{ID: "private-model-9287", SupportsReasoning: &enabled, ReasoningEfforts: values}},
	}
	if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{Action: "upsert", ExpectedRevision: snapshot.Revision, Provider: &profile}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &activationTestProvider{name: profile.ID, model: profile.Model}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = p.model
	if err := store.WriteHeaderFull(session.Header{ID: "private-effort-session", Provider: p.name, Model: p.model}); err != nil {
		t.Fatal(err)
	}
	return NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "private-effort-session", ProviderName: p.name}), loop
}

func effortSettingsHTTP(t *testing.T, server *Server, method, input string, wantStatus int) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(method, "/api/effort", bytes.NewBufferString(input)))
	if rr.Code != wantStatus {
		t.Fatalf("effort HTTP %d want %d: %s", rr.Code, wantStatus, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestModelEffortSettingsEnableUnknownModelAndValidateDeclaredOptions(t *testing.T) {
	s, loop := modelEffortSettingsFixture(t, true, map[string]string{"high": "xhigh"})
	response := effortSettingsHTTP(t, s, http.MethodGet, "", http.StatusOK)
	if response["supported"] != true || !reflect.DeepEqual(response["options"], []any{"default", "high"}) || response["applies"] != "next model request" {
		t.Fatalf("private model options: %#v", response)
	}
	for _, choice := range []string{"low", "medium", "xhigh"} {
		effortSettingsHTTP(t, s, http.MethodPost, `{"effort":"`+choice+`"}`, http.StatusBadRequest)
		if loop.EffortValue() != llm.EffortDefault {
			t.Fatal("rejected choice changed live effort")
		}
	}
	response = effortSettingsHTTP(t, s, http.MethodPost, `{"effort":"high"}`, http.StatusOK)
	if response["effort"] != "high" || response["applies"] != "next model request" || loop.EffortValue() != llm.EffortHigh {
		t.Fatalf("accepted mapped slot not canonical: %#v", response)
	}
	header, _, err := s.store.LoadHeader("private-effort-session")
	if err != nil || header.Effort != "high" {
		t.Fatalf("canonical effort not durable: %+v error=%v", header, err)
	}
	effortSettingsHTTP(t, s, http.MethodPost, `{"effort":"default"}`, http.StatusOK)
	if loop.EffortValue() != llm.EffortDefault {
		t.Fatal("default did not clear explicit effort")
	}
	// A separate per-turn process receives its next setting from the durable
	// session header. No control message currently changes an in-flight worker.
	s.isolatedRunner = &blockingIsolatedRunner{}
	response = effortSettingsHTTP(t, s, http.MethodPost, `{"effort":"high"}`, http.StatusOK)
	if response["applies"] != "next turn" {
		t.Fatalf("isolated-worker application boundary overstated: %#v", response)
	}
}

func TestModelEffortSettingsDisableLegacyChoiceAndStayRouteScoped(t *testing.T) {
	t.Run("explicit enable with legacy slots", func(t *testing.T) {
		s, _ := modelEffortSettingsFixture(t, true, nil)
		response := effortSettingsHTTP(t, s, http.MethodGet, "", http.StatusOK)
		if response["supported"] != true || !reflect.DeepEqual(response["options"], []any{"default", "low", "medium", "high"}) {
			t.Fatalf("explicit enabled model lost existing slots: %#v", response)
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		s, loop := modelEffortSettingsFixture(t, false, nil)
		loop.SetEffort(llm.EffortHigh)
		response := effortSettingsHTTP(t, s, http.MethodGet, "", http.StatusOK)
		if response["supported"] != false || response["effort"] != "default" || !reflect.DeepEqual(response["options"], []any{"default"}) {
			t.Fatalf("disabled model retained effective choice: %#v", response)
		}
		effortSettingsHTTP(t, s, http.MethodPost, `{"effort":"high"}`, http.StatusConflict)
	})
	t.Run("exact binding", func(t *testing.T) {
		_, _ = modelEffortSettingsFixture(t, true, map[string]string{"high": "xhigh"})
		cfg, _, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cap := reasoningEffortCapability(cfg, "private-route", "private-model-9287"); !cap.Supported {
			t.Fatalf("exact fixture declaration unavailable: %+v", cap)
		}
		if cap := reasoningEffortCapability(cfg, "private-route", "another-private-model"); cap.Supported {
			t.Fatal("reasoning declaration leaked to another model")
		}
		raw := cfg.Provider.Custom["private-route"]
		cfg.Provider.Custom["another-route"] = raw
		if cap := reasoningEffortCapability(cfg, "another-route", "private-model-9287"); cap.Supported {
			t.Fatal("reasoning declaration leaked to another provider")
		}
		original := raw
		raw.BaseURL = "https://different.example/v1"
		cfg.Provider.Custom["private-route"] = raw
		if cap := reasoningEffortCapability(cfg, "private-route", "private-model-9287"); cap.Supported {
			t.Fatal("reasoning declaration leaked to another endpoint")
		}
		original.Transport = "openai_responses"
		cfg.Provider.Custom["private-route"] = original
		if cap := reasoningEffortCapability(cfg, "private-route", "private-model-9287"); cap.Supported {
			t.Fatal("reasoning declaration leaked to another transport")
		}
	})
}

func TestModelEffortSettingsLeaveCloudCredentialSyntaxOnExistingAutoPath(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	for _, transport := range []string{"azure_openai", "vertex_anthropic", "bedrock_anthropic"} {
		t.Run(transport, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Provider.Custom = map[string]config.ProviderRaw{"cloud-route": {Transport: transport, BaseURL: "resource-name", Model: "gpt-5"}}
			if _, found, err := savedReasoningDeclaration(cfg, "cloud-route", "gpt-5"); err != nil || found {
				t.Fatalf("read-only cloud profile reached API-key sidecar normalization: found=%v err=%v", found, err)
			}
		})
	}
}
