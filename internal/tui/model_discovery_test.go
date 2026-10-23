package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

func TestEndpointModelsAppearInModelPicker(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("missing route credential")
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6.1-sol"},{"id":"claude-opus-5-5-high"},{"id":"kimi-k3"}]}`)
	}))
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{"easycliproxyapi": {Transport: "openai_chat", BaseURL: server.URL + "/v1", Model: "gpt-6.1-sol", APIKey: "fixture-key"}}
	m := &Model{cfg: cfg}
	check := func() {
		t.Helper()
		seen := map[string]int{}
		for _, c := range m.modelPickerChoices(false) {
			if c.Provider == "easycliproxyapi" {
				seen[c.ID]++
			}
		}
		for _, id := range []string{"gpt-6.1-sol", "claude-opus-5-5-high", "kimi-k3"} {
			if seen[id] != 1 {
				t.Fatalf("model %s count = %d; choices: %v", id, seen[id], seen)
			}
		}
		if cfg.Provider.Custom["easycliproxyapi"].Model != "gpt-6.1-sol" {
			t.Fatal("picker changed default")
		}
	}
	check()
	server.Close()
	check() // Validated disk cache remains usable while endpoint is unavailable.
	cfg.Provider.Custom["easycliproxyapi"] = config.ProviderRaw{Transport: "openai_chat", BaseURL: server.URL + "/other", Model: "gpt-6.1-sol", APIKey: "fixture-key"}
	for _, c := range m.modelPickerChoices(false) {
		if c.Provider == "easycliproxyapi" && c.ID == "kimi-k3" {
			t.Fatal("cache leaked across endpoints")
		}
	}
}

func TestLiveEasyCLIModelPicker(t *testing.T) {
	// Opt-in local acceptance check; never sends an inference request.
	if os.Getenv("METIS_TEST_LIVE_EASYCLI") != "1" {
		t.Skip("opt-in live acceptance")
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Provider.Custom["easycliproxyapi"]; !ok {
		t.Skip("EasyCLI not configured")
	}
	choices := (&Model{cfg: cfg}).modelPickerChoices(false)
	raw := cfg.Provider.Custom["easycliproxyapi"]
	client, err := modelinfo.New(filepath.Join(config.Home(), "cache", "provider-models"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cfg.ResolveAPIKey("easycliproxyapi")
	if err != nil {
		t.Fatal(err)
	}
	models, err := client.RefreshIfStale(context.Background(), modelinfo.Binding{ProviderID: "easycliproxyapi", Transport: raw.Transport, BaseURL: raw.BaseURL}, key)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range choices {
		if c.Provider == "easycliproxyapi" {
			seen[c.ID] = true
		}
	}
	for _, model := range models {
		if !seen[model.ID] {
			t.Errorf("endpoint model missing from picker: %s", model.ID)
		}
	}
	t.Logf("EasyCLI endpoint=%d models; picker=%d EasyCLI models", len(models), len(seen))
}
