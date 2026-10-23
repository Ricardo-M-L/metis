package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func registerMembershipFixture(t *testing.T, profiles ...config.ProviderSettingsProvider) {
	t.Helper()
	for _, profile := range profiles {
		snapshot, err := config.ReadProviderSettingsForWorkspace(false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{Action: "upsert", ExpectedRevision: snapshot.Revision, Provider: &profile}); err != nil {
			t.Fatal(err)
		}
	}
}

func membershipOpenAIBody(revision, intent string) map[string]any {
	return map[string]any{"expectedRevision": revision, "intent": intent, "id": "openai", "transport": "openai_chat", "baseUrl": "https://api.openai.com/v1", "displayName": "Added OpenAI", "model": "member-model", "models": []config.ProviderModelSettings{{ID: "member-model", OutputBudget: 512}}, "credentialAction": "keep"}
}

func membershipModels(t *testing.T, s *Server) []webModel {
	t.Helper()
	rr, _ := settingsAPI(t, s.handler(), http.MethodGet, "/api/models", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("models %d %s", rr.Code, rr.Body.String())
	}
	var response struct {
		Models []webModel `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Models
}

func TestProviderMembershipEmptyAddWithoutKeyEditDeleteAndRestart(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		name := "untrusted workspace"
		if trusted {
			name = "trusted workspace"
		}
		t.Run(name, func(t *testing.T) {
			isolatedWebProviderConfig(t)
			s, store := testServer(t)
			s.trustProviderConfig = trusted
			s.activeProviderName, s.activeModel = "anthropic", "implicit-default"
			rr, initial := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers", nil)
			if rr.Code != http.StatusOK || len(initial.Providers) != 0 || initial.SavedDefault.Provider != "" || initial.Current.Provider != "" || len(membershipModels(t, s)) != 0 {
				t.Fatalf("fresh Desktop exposed transport presets as saved providers: %d %s", rr.Code, rr.Body.String())
			}
			body := membershipOpenAIBody(initial.Revision, "create")
			rr, created := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers", body)
			if rr.Code != http.StatusCreated || len(created.Providers) != 1 {
				t.Fatalf("add without key: %d %s", rr.Code, rr.Body.String())
			}
			view := created.Providers[0]
			if view.ID != "openai" || view.Custom || !view.Editable || !view.Removable || view.CredentialConfigured || created.SavedDefault.Provider != "" {
				t.Fatalf("explicit membership confused with transport or credential state: %+v", created)
			}
			models := membershipModels(t, s)
			if len(models) != 1 || models[0].Provider != "openai" || models[0].Model != "member-model" || models[0].CredentialConfigured {
				t.Fatalf("configured model disappeared without a key: %+v", models)
			}
			restarted := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{TrustProviderConfig: trusted})
			_, persisted := settingsAPI(t, restarted.handler(), http.MethodGet, "/api/providers", nil)
			if len(persisted.Providers) != 1 || persisted.Providers[0].Model != "member-model" {
				t.Fatalf("restart lost explicit no-key registration: %+v", persisted)
			}
			body["expectedRevision"], body["intent"], body["displayName"] = persisted.Revision, "update", "Edited OpenAI"
			body["models"] = []config.ProviderModelSettings{{ID: "member-model", OutputBudget: 768}, {ID: "second-member"}}
			rr, updated := settingsAPI(t, restarted.handler(), http.MethodPost, "/api/providers", body)
			if rr.Code != http.StatusOK || len(updated.Providers[0].Models) != 2 {
				t.Fatalf("edit registration: %d %s", rr.Code, rr.Body.String())
			}
			rr, _ = settingsAPI(t, restarted.handler(), http.MethodDelete, "/api/providers", map[string]any{"id": "openai", "expectedRevision": created.Revision})
			if rr.Code != http.StatusConflict {
				t.Fatalf("stale deletion: %d %s", rr.Code, rr.Body.String())
			}
			rr, deleted := settingsAPI(t, restarted.handler(), http.MethodDelete, "/api/providers", map[string]any{"id": "openai", "expectedRevision": updated.Revision})
			if rr.Code != http.StatusOK || len(deleted.Providers) != 0 {
				t.Fatalf("remove added built-in transport: %d %s", rr.Code, rr.Body.String())
			}
			p := &activationTestProvider{name: "openai", model: "member-model"}
			loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "historical prompt", 2)
			loop.Model = p.model
			if err := store.WriteHeaderFull(session.Header{ID: "removed-member-history", Provider: p.name, Model: p.model, System: "historical prompt", Status: "idle"}); err != nil {
				t.Fatal(err)
			}
			buildCalls := 0
			after := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "removed-member-history", ProviderName: p.name, TrustProviderConfig: trusted, BuildProvider: func(provider, model string) (*rtpkg.ProviderBuild, error) {
				buildCalls++
				return &rtpkg.ProviderBuild{Provider: &activationTestProvider{name: provider, model: model}, Model: model}, nil
			}})
			_, afterRestart := settingsAPI(t, after.handler(), http.MethodGet, "/api/providers", nil)
			if len(afterRestart.Providers) != 0 || afterRestart.SavedDefault.Provider != "" || afterRestart.Current.Provider != "openai" || len(membershipModels(t, after)) != 0 {
				t.Fatalf("removed provider revived or historical route was hidden: %+v", afterRestart)
			}
			for _, action := range []struct {
				path string
				body any
			}{{"/api/models", map[string]string{"provider": "openai", "model": "member-model"}}, {"/api/sessions/activate", map[string]string{"id": "removed-member-history"}}, {"/api/turns", map[string]string{"sessionId": "removed-member-history", "input": "must not execute"}}, {"/api/sessions", map[string]string{}}} {
				rr, _ := settingsAPI(t, after.handler(), http.MethodPost, action.path, action.body)
				if rr.Code != http.StatusConflict {
					t.Fatalf("removed membership bypassed %s: %d %s", action.path, rr.Code, rr.Body.String())
				}
			}
			if buildCalls != 0 {
				t.Fatal("removed provider reached the builder")
			}
			header, _, err := store.LoadHeader("removed-member-history")
			if err != nil || header.Status != "idle" || header.System != "historical prompt" || header.Model != "member-model" {
				t.Fatalf("rejected admission changed historical session: %+v %v", header, err)
			}
		})
	}
}

func TestProviderMembershipDefaultAndOAuthRemoval(t *testing.T) {
	t.Run("saved default remains protected", func(t *testing.T) {
		isolatedWebProviderConfig(t)
		s, _ := testServer(t)
		_, initial := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers", nil)
		_, created := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers", membershipOpenAIBody(initial.Revision, "create"))
		rr, selected := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers/default", map[string]any{"id": "openai", "expectedRevision": created.Revision})
		if rr.Code != http.StatusOK || selected.SavedDefault.Provider != "openai" || !selected.Providers[0].Removable {
			t.Fatalf("default selection/removal state: %d %s", rr.Code, rr.Body.String())
		}
		rr, _ = settingsAPI(t, s.handler(), http.MethodDelete, "/api/providers", map[string]any{"id": "openai", "expectedRevision": selected.Revision})
		if rr.Code != http.StatusConflict {
			t.Fatalf("default deletion: %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("OAuth provider is removed without preset resurrection", func(t *testing.T) {
		isolatedWebProviderConfig(t)
		putWebCodexOAuth(t)
		s, store := testServer(t)
		_, snapshot := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers", nil)
		if len(snapshot.Providers) != 1 || snapshot.Providers[0].ID != "openai-codex" || !snapshot.Providers[0].Removable {
			t.Fatalf("saved OAuth membership: %+v", snapshot)
		}
		rr, _ := settingsAPI(t, s.handler(), http.MethodDelete, "/api/providers", map[string]any{"id": "openai-codex", "expectedRevision": snapshot.Revision})
		if rr.Code != http.StatusOK {
			t.Fatalf("OAuth removal: %d %s", rr.Code, rr.Body.String())
		}
		if credential, err := auth.GetOAuth("openai-codex"); err != nil || credential != nil {
			t.Fatalf("OAuth removal left credential: present=%v error=%v", credential != nil, err)
		}
		restarted := NewServer("127.0.0.1:0", nil, store)
		_, final := settingsAPI(t, restarted.handler(), http.MethodGet, "/api/providers", nil)
		if len(final.Providers) != 0 || len(membershipModels(t, restarted)) != 0 {
			t.Fatalf("OAuth preset revived on restart: %+v", final)
		}
	})
}

func TestProviderMembershipRespectsWorkspaceTrust(t *testing.T) {
	isolatedWebProviderConfig(t)
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0700); err != nil {
		t.Fatal(err)
	}
	data := `[provider.openai]
model = "workspace-model"
[provider.custom.workspace-only]
transport = "openai_chat"
base_url = "https://workspace.example/v1"
model = "workspace-only-model"
`
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, store := testServer(t)
	for _, trusted := range []bool{false, true} {
		s := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{TrustProviderConfig: trusted})
		rr, snapshot := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers", nil)
		want := 0
		if trusted {
			want = 2
		}
		if rr.Code != http.StatusOK || len(snapshot.Providers) != want || len(membershipModels(t, s)) != want {
			t.Fatalf("workspace trust=%v: %d %s", trusted, rr.Code, rr.Body.String())
		}
		for _, provider := range snapshot.Providers {
			if !provider.ReadOnly || provider.Removable || provider.RemoveReason == "" {
				t.Fatalf("workspace registration claimed user ownership: %+v", provider)
			}
		}
	}
}

func TestProviderSettingsReadsPreserveUnusableAndLegacyCredentialMembership(t *testing.T) {
	isolatedWebProviderConfig(t)
	const unsafeToken = "sk-ant-oat-disabled-review-fixture"
	if err := auth.Save(auth.File{"anthropic": {Type: "api", Key: unsafeToken}, "other": {Type: "api", Key: "keep-other-fixture"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.Home(), "auth.json"), []byte(`{"legacy-other":{"type":"api","key":"keep-legacy-other-fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.Home(), "llm-oauth.json"), []byte(`{"format_version":1,"credentials":{"legacy-oauth-other":{"access_token":"keep-legacy-oauth-fixture"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	type protectedFile struct {
		exists bool
		data   []byte
	}
	guard := map[string]protectedFile{}
	for _, path := range []string{auth.Path(), auth.OAuthPath(), filepath.Join(config.Home(), "auth.json"), filepath.Join(config.Home(), "llm-oauth.json")} {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		guard[path] = protectedFile{exists: err == nil, data: data}
	}
	s, _ := testServer(t)
	var revision string
	for round := 0; round < 2; round++ {
		rr, snapshot := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers", nil)
		if rr.Code != http.StatusOK || len(snapshot.Providers) != 1 || snapshot.Providers[0].ID != "anthropic" || snapshot.Providers[0].CredentialConfigured {
			t.Fatalf("read erased or activated an unsafe saved credential: round=%d HTTP=%d %s", round, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), unsafeToken) || strings.Contains(rr.Body.String(), "keep-other-fixture") {
			t.Fatal("read returned credential contents")
		}
		if round == 0 {
			revision = snapshot.Revision
		} else if revision != snapshot.Revision {
			t.Fatal("read-only GET changed the CAS revision")
		}
		models := membershipModels(t, s)
		if len(models) != 1 || models[0].Provider != "anthropic" || models[0].CredentialConfigured {
			t.Fatalf("model picker discarded unusable saved membership: %+v", models)
		}
		cfg, err := s.loadProviderConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !s.providerExists(cfg, "anthropic") {
			t.Fatal("membership probe discarded unsafe saved membership")
		}
	}
	for path, before := range guard {
		after, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if (err == nil) != before.exists || !bytes.Equal(after, before.data) {
			t.Fatalf("settings/model/membership read mutated credential presence or bytes: %s", filepath.Base(path))
		}
	}
}

func TestProviderSettingsUntrustedWorkspaceCreateAndSaveUseTheSameScope(t *testing.T) {
	isolatedWebProviderConfig(t)
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0700); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(project, ".metis", "config.toml")
	projectBytes := []byte("[provider]\ndefault = \"openai\"\n[provider.openai]\nmodel = \"project-model\"\nbase_url = \"https://project.example/v1\"\n")
	if err := os.WriteFile(projectPath, projectBytes, 0600); err != nil {
		t.Fatal(err)
	}
	s, store := testServer(t)
	_, initial := settingsAPI(t, s.handler(), http.MethodGet, "/api/providers", nil)
	if len(initial.Providers) != 0 {
		t.Fatalf("untrusted project entered user settings: %+v", initial)
	}
	body := membershipOpenAIBody(initial.Revision, "create")
	rr, created := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusCreated || len(created.Providers) != 1 || created.Providers[0].ReadOnly || created.Providers[0].BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("untrusted project blocked user-owned create: %d %s", rr.Code, rr.Body.String())
	}
	body["expectedRevision"], body["intent"], body["displayName"] = created.Revision, "update", "User-owned OpenAI"
	rr, updated := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("untrusted project blocked user-owned edit: %d %s", rr.Code, rr.Body.String())
	}
	rr, selected := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers/default", map[string]any{"id": "openai", "expectedRevision": updated.Revision})
	if rr.Code != http.StatusOK || selected.SavedDefault.Provider != "openai" {
		t.Fatalf("untrusted project blocked user-owned default: %d %s", rr.Code, rr.Body.String())
	}
	rr, modelSaved := settingsAPI(t, s.handler(), http.MethodPost, "/api/providers/model", map[string]any{"id": "openai", "model": "user-next-model", "expectedRevision": selected.Revision})
	if rr.Code != http.StatusOK || modelSaved.Providers[0].Model != "user-next-model" {
		t.Fatalf("legacy model save used a different trust scope: %d %s", rr.Code, rr.Body.String())
	}
	trusted := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{TrustProviderConfig: true})
	_, trustedSnapshot := settingsAPI(t, trusted.handler(), http.MethodGet, "/api/providers", nil)
	if len(trustedSnapshot.Providers) != 1 || !trustedSnapshot.Providers[0].ReadOnly || trustedSnapshot.Providers[0].Model != "project-model" {
		t.Fatalf("trusted project lost its separate effective route: %+v", trustedSnapshot)
	}
	body["expectedRevision"] = trustedSnapshot.Revision
	rr, _ = settingsAPI(t, trusted.handler(), http.MethodPost, "/api/providers", body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("trusted workspace ownership was bypassed: %d %s", rr.Code, rr.Body.String())
	}
	actual, err := os.ReadFile(projectPath)
	if err != nil || !bytes.Equal(actual, projectBytes) {
		t.Fatalf("user settings changed project configuration: %v", err)
	}
}
