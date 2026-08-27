package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestCommitActiveModelSelectionPersistsSessionHeader(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "model-switch-session"
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: "bigmodel", Model: "glm-5.3", System: "system",
	}); err != nil {
		t.Fatal(err)
	}

	provider := &activationTestProvider{name: "wire", model: "glm-5.3"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "sensenova-6.8-flash-lite"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID,
		ProviderName:     "bigmodel",
	})

	if err := s.commitActiveModelSelection("sensenova", loop.Model); err != nil {
		t.Fatalf("commitActiveModelSelection: %v", err)
	}
	hdr, _, err := store.LoadHeader(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Provider != "sensenova" || hdr.Model != "sensenova-6.8-flash-lite" {
		t.Fatalf("persisted provider/model = %q/%q", hdr.Provider, hdr.Model)
	}
}

func modelSwitchFixture(t *testing.T, storeRoot string) (*Server, *agent.Loop, *activationTestProvider, string, string) {
	t.Helper()
	store, err := session.NewStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "web-model-switch"
	sections := []llm.SystemSection{{Name: "identity", Body: "BASE IDENTITY", Cache: true}}
	oldSystem, oldSections := rtpkg.RebindProviderPrompt("", sections, "openai", "gpt-old")
	oldProvider := &activationTestProvider{name: "openai", model: "gpt-old"}
	loop := agent.NewLoop(oldProvider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, oldSystem, 2)
	loop.Model = oldProvider.model
	loop.SystemSections = oldSections
	loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), oldProvider.model, oldProvider.MaxContextTokens(), oldProvider)
	loop.Compactor.MaxOutputTokens = 16_000
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: oldProvider.name, Model: oldProvider.model, System: oldSystem,
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID,
		ProviderName:     oldProvider.name,
		BuildProvider: func(providerName, model string) (*rtpkg.ProviderBuild, error) {
			return &rtpkg.ProviderBuild{
				Provider:        &activationTestProvider{name: providerName, model: model},
				Model:           model,
				MaxOutputTokens: 4_096,
			}, nil
		},
	})
	return server, loop, oldProvider, oldSystem, sessionID
}

func TestWebModelSwitchRebindsPromptAndTargetOutputBudget(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	storeRoot := t.TempDir()
	server, loop, oldProvider, oldSystem, sessionID := modelSwitchFixture(t, storeRoot)

	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(
		http.MethodPost, "/api/models", bytes.NewBufferString(`{"provider":"anthropic","model":"claude-new"}`),
	))
	if rr.Code != http.StatusOK {
		t.Fatalf("model switch = %d: %s", rr.Code, rr.Body.String())
	}
	runtimeState := loop.ProviderRuntimeState()
	if runtimeState.Provider == oldProvider || runtimeState.Provider.Name() != "anthropic" || runtimeState.Model != "claude-new" {
		t.Fatalf("runtime routing not switched: %+v", runtimeState)
	}
	if runtimeState.MaxOutputTokens != 4_096 {
		t.Fatalf("target output budget = %d, want 4096", runtimeState.MaxOutputTokens)
	}
	oldHint := rtpkg.ProviderHintFor("openai", "gpt-old")
	newHint := rtpkg.ProviderHintFor("anthropic", "claude-new")
	if newHint == "" || !strings.Contains(runtimeState.System, newHint) || strings.Contains(runtimeState.System, oldHint) {
		t.Fatalf("provider prompt was not rebound\nold:\n%s\nnew:\n%s", oldSystem, runtimeState.System)
	}
	store := server.store
	hdr, _, err := store.LoadHeader(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Provider != "anthropic" || hdr.Model != "claude-new" || hdr.System != runtimeState.System {
		t.Fatalf("persisted switch differs from runtime: %+v", hdr)
	}
}

func TestActivateSessionRestoresTypedSectionsFromCanonicalReboundPrompt(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	server, loop, _, _, _ := modelSwitchFixture(t, t.TempDir())

	const (
		targetID       = "resumed-rebound-prompt"
		targetProvider = "anthropic"
		targetModel    = "claude-resumed"
	)
	canonicalSystem, canonicalSections := rtpkg.RebindProviderPrompt(
		server.freshSystem, server.freshSystemSections, targetProvider, targetModel,
	)
	if len(canonicalSections) == 0 {
		t.Fatal("fixture did not produce typed provider sections")
	}
	if err := server.store.WriteHeaderFull(session.Header{
		ID: targetID, Provider: targetProvider, Model: targetModel, System: canonicalSystem,
	}); err != nil {
		t.Fatal(err)
	}
	hdr, history, err := server.store.Load(targetID)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.activateSession(targetID, hdr, history); err != nil {
		t.Fatalf("activateSession: %v", err)
	}

	resumed := loop.ProviderRuntimeState()
	if resumed.System != canonicalSystem || !reflect.DeepEqual(resumed.SystemSections, canonicalSections) {
		t.Fatalf("canonical flattened prompt did not recover typed sections\nsystem=%q\nsections=%+v",
			resumed.System, resumed.SystemSections)
	}

	// A second switch is the behavior that used to expose the loss: without
	// sections, RebindProviderPrompt cannot identify and replace provider_hint.
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(
		http.MethodPost, "/api/models", bytes.NewBufferString(`{"provider":"deepseek","model":"deepseek-v4-pro"}`),
	))
	if rr.Code != http.StatusOK {
		t.Fatalf("second model switch = %d: %s", rr.Code, rr.Body.String())
	}
	after := loop.ProviderRuntimeState()
	oldHint := rtpkg.ProviderHintFor(targetProvider, targetModel)
	newHint := rtpkg.ProviderHintFor("deepseek", "deepseek-v4-pro")
	if newHint == "" || !strings.Contains(after.System, newHint) || strings.Contains(after.System, oldHint) {
		t.Fatalf("second switch retained stale provider prompt\nold hint present=%v\nnew hint present=%v",
			strings.Contains(after.System, oldHint), strings.Contains(after.System, newHint))
	}
	if len(after.SystemSections) == 0 {
		t.Fatal("second switch flattened the recovered typed prompt")
	}
}

func TestWebModelSwitchPersistenceFailureLeavesRuntimeUntouched(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	storeRoot := t.TempDir()
	server, loop, _, _, _ := modelSwitchFixture(t, storeRoot)
	before := loop.ProviderRuntimeState()
	server.stateMu.RLock()
	beforeProviderName, beforeModel := server.activeProviderName, server.activeModel
	server.stateMu.RUnlock()

	if err := os.RemoveAll(storeRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storeRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(
		http.MethodPost, "/api/models", bytes.NewBufferString(`{"provider":"anthropic","model":"claude-new"}`),
	))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("model switch failure = %d: %s", rr.Code, rr.Body.String())
	}
	after := loop.ProviderRuntimeState()
	if after.Provider != before.Provider || after.Model != before.Model || after.ContextWindow != before.ContextWindow ||
		after.MaxOutputTokens != before.MaxOutputTokens || after.System != before.System ||
		!reflect.DeepEqual(after.SystemSections, before.SystemSections) {
		t.Fatalf("failed persistence mutated runtime\nbefore=%+v\nafter=%+v", before, after)
	}
	server.stateMu.RLock()
	afterProviderName, afterModel := server.activeProviderName, server.activeModel
	server.stateMu.RUnlock()
	if afterProviderName != beforeProviderName || afterModel != beforeModel {
		t.Fatalf("failed persistence mutated selector: %q/%q -> %q/%q",
			beforeProviderName, beforeModel, afterProviderName, afterModel)
	}
}
