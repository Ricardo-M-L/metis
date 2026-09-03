package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestEffortAPIIsCapabilityGatedAndPersistsSessionChoice(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(filepath.Join(os.Getenv("METIS_HOME"), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "openai", model: "gpt-5-private-reasoning-model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = provider.model
	if err := store.WriteHeaderFull(session.Header{ID: "effort-session", Provider: "openai", Model: provider.model, Effort: "default"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "effort-session", ProviderName: "openai",
	})
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/effort", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"supported":true`)) {
		t.Fatalf("effort capability: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/effort", bytes.NewBufferString(`{"effort":"high"}`)))
	if rr.Code != http.StatusOK || loop.EffortValue() != llm.EffortHigh {
		t.Fatalf("set effort: %d %s value=%q", rr.Code, rr.Body.String(), loop.EffortValue())
	}
	hdr, _, err := store.LoadHeader("effort-session")
	if err != nil || hdr.Effort != "high" {
		t.Fatalf("persisted effort = %q err=%v", hdr.Effort, err)
	}

	// A running turn owns runMu for its whole lifetime, but effort is a live
	// preference: the next model request in that turn must observe the update.
	s.runMu.Lock()
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/effort", bytes.NewBufferString(`{"effort":"low"}`)))
	s.runMu.Unlock()
	if rr.Code != http.StatusOK || loop.EffortValue() != llm.EffortLow || !bytes.Contains(rr.Body.Bytes(), []byte(`"applies":"next model request"`)) {
		t.Fatalf("running-turn effort update = %d %s value=%q", rr.Code, rr.Body.String(), loop.EffortValue())
	}

	// Persistence is the commit point: a disk failure must leave the live
	// request preference untouched rather than creating a resume-time split.
	if err := os.RemoveAll(store.Dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/effort", bytes.NewBufferString(`{"effort":"medium"}`)))
	if rr.Code != http.StatusInternalServerError || loop.EffortValue() != llm.EffortLow {
		t.Fatalf("failed effort persistence = %d %s value=%q", rr.Code, rr.Body.String(), loop.EffortValue())
	}

	unsupported := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "gemini"})
	unsupported.stateMu.Lock()
	unsupported.activeProviderName = "gemini"
	unsupported.activeModel = "gemini-2.5-pro"
	unsupported.stateMu.Unlock()
	rr = httptest.NewRecorder()
	unsupported.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/effort", bytes.NewBufferString(`{"effort":"low"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("unsupported effort status = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPresetAPIHasRecoverableCustomLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	opened := ""
	s, store := testServer(t)
	s = NewServer("127.0.0.1:0", nil, store, RuntimeBindings{OpenPath: func(path string) error { opened = path; return nil }})
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/presets", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"id":"standard"`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"id":"plan"`)) {
		t.Fatalf("preset list: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/presets", bytes.NewBufferString(`{"source":"plan","target":"plan-copy"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("copy preset: %d %s", rr.Code, rr.Body.String())
	}
	copyPath := filepath.Join(home, "agents", "plan-copy.md")
	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode=%v", info.Mode().Perm())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/presets/default", bytes.NewBufferString(`{"id":"plan-copy"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("default preset: %d %s", rr.Code, rr.Body.String())
	}
	launch, err := LoadDesktopLaunchPreferences()
	if err != nil || launch.DefaultPreset != "plan-copy" {
		t.Fatalf("launch preset = %+v err=%v", launch, err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/presets", bytes.NewBufferString(`{"id":"plan-copy"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete default preset = %d: %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/presets/default", bytes.NewBufferString(`{"id":"standard"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("restore standard default: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/presets", bytes.NewBufferString(`{"id":"plan-copy"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("recoverableAt")) {
		t.Fatalf("recoverable delete: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("source preset still exists: %v", err)
	}
	trash, err := filepath.Glob(filepath.Join(home, "agents", ".trash", "plan-copy-*.md"))
	if err != nil || len(trash) != 1 {
		t.Fatalf("recoverable copy = %v err=%v", trash, err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/presets/open", nil))
	if rr.Code != http.StatusOK || opened != filepath.Join(home, "agents") {
		t.Fatalf("open preset dir: %d %s opened=%q", rr.Code, rr.Body.String(), opened)
	}
}

func TestPluginAPIListsManifestWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	root := filepath.Join(home, "plugins", "demo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `manifest_version = 1
name = "demo"
version = "1.2.3"
description = "safe plugin"

[mcp_server]
command = "demo-server"
[mcp_server.env]
SECRET_TOKEN = "must-not-leak"
`
	if err := os.WriteFile(filepath.Join(root, "plugin.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plugins", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"name":"demo"`)) || !bytes.Contains(rr.Body.Bytes(), []byte("MCP tools")) {
		t.Fatalf("plugin list: %d %s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("must-not-leak")) || bytes.Contains(rr.Body.Bytes(), []byte("SECRET_TOKEN")) {
		t.Fatalf("plugin response leaked environment: %s", rr.Body.String())
	}
}

func TestRoutingOverviewIsReadOnlyAndCredentialFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"openai":"sk-routing-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	s.stateMu.Lock()
	s.activeProviderName = "openai"
	s.activeModel = "gpt-5-test"
	s.stateMu.Unlock()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/routing", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"rules"`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"mode":"manual"`)) {
		t.Fatalf("routing overview: %d %s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("sk-routing-secret")) {
		t.Fatalf("routing overview leaked credential: %s", rr.Body.String())
	}
}

func TestSessionActivateRestoresEffortBeforeComposerMutation(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "openai", model: "gpt-5-test"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = provider.model
	if err := store.WriteHeaderFull(session.Header{ID: "resume-effort", Provider: "openai", Model: provider.model, System: "system", Effort: "high", Preset: "plan"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("resume-effort", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{ProviderName: "openai"})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/activate", bytes.NewBufferString(`{"id":"resume-effort"}`)))
	if rr.Code != http.StatusOK || loop.EffortValue() != llm.EffortHigh {
		t.Fatalf("activate: %d %s effort=%q", rr.Code, rr.Body.String(), loop.EffortValue())
	}
	s.stateMu.RLock()
	preset := s.activePreset
	s.stateMu.RUnlock()
	if preset != "plan" {
		t.Fatalf("active preset = %q", preset)
	}
}
