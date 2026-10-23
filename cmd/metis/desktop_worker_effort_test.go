package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// Use the real worker startup flags, durable header patches, runtime builder,
// request loop and HTTP adapter. Checking only the parent's effort API misses
// the process boundary at which the selected value used to disappear.
func TestDesktopWorkerStartupRestoresSessionEffortToPhysicalRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		cli        string
		profile    bool
		wantEffort llm.Effort
		wantWire   string
	}{
		{name: "stored high", header: "high", wantEffort: llm.EffortHigh, wantWire: "xhigh"},
		{name: "explicit CLI wins", header: "high", cli: "low", wantEffort: llm.EffortLow, wantWire: "low"},
		{name: "explicit CLI default", header: "high", cli: "default"},
		{name: "stored default beats profile fallback", header: "default", profile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateResumeRuntimeTest(t)
			t.Chdir(t.TempDir())
			for key, value := range map[string]string{
				"METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1", "METIS_NO_UPDATE_CHECK": "1",
				"METIS_DESKTOP_ISOLATED_WORKER": "1", "METIS_RUN_CACHE": "0", "METIS_DEBUG": "0",
				"METIS_RECOVERY_MAX_SECONDS": "0", "METIS_VERIFICATION_URL": "", "METIS_VERIFICATION_TOKEN": "",
			} {
				t.Setenv(key, value)
			}
			bodies := make(chan map[string]any, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					fmt.Fprint(w, `{"data":[{"id":"private-worker-model","context_window":1000000}]}`)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					http.NotFound(w, r)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode physical request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				bodies <- body
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			enabled := true
			profile := config.ProviderSettingsProvider{
				ID: "worker-effort-route", Transport: "openai_chat", BaseURL: server.URL + "/v1", Model: "private-worker-model",
				Models: []config.ProviderModelSettings{{ID: "private-worker-model", ContextWindow: 1000000, OutputBudget: 1024,
					SupportsReasoning: &enabled, ReasoningEfforts: map[string]string{"low": "low", "high": "xhigh"}}},
			}
			snapshot, err := config.ReadProviderSettings()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{Action: "upsert", ExpectedRevision: snapshot.Revision, Provider: &profile, APIKey: "test-worker-key"}); err != nil {
				t.Fatal(err)
			}
			store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			const id = "desktop-effort-owned-session"
			if err := store.WriteHeaderFull(session.Header{ID: id, Provider: profile.ID, Model: profile.Model, System: "fixture system"}); err != nil {
				t.Fatal(err)
			}
			if err := store.WriteHeaderFull(session.Header{ID: id, Effort: tc.header}); err != nil {
				t.Fatal(err)
			}
			args := []string{"--resume", id, "--desktop-worker", "--streamlined", "--bare", "--no-auth-wizard", "--tools", "Read"}
			if tc.cli != "" {
				args = append(args, "--effort", tc.cli)
			}
			if tc.profile {
				if err := os.MkdirAll(filepath.Join(config.Home(), "agents"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config.Home(), "agents", "effort-profile.md"), []byte("---\nname: effort-profile\neffort: high\n---\nFixture profile.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--agent", "effort-profile")
			}
			flags, _, err := parseFlags(args)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rt, err := setupRuntime(ctx, flags)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Cleanup()
			// Continue to the wire even when startup is wrong, so the regression
			// reports the missing physical parameter alongside the lost state.
			gotEffort := rt.loop.EffortValue()
			rt.loop.AppendUser("Reply done without tools.")
			if _, err := collectHeadlessEvents(func(events chan<- agent.Event) error { return rt.loop.Run(ctx, events) }); err != nil {
				t.Fatal(err)
			}
			select {
			case body := <-bodies:
				value, present := body["reasoning_effort"]
				if gotEffort != tc.wantEffort || tc.wantWire == "" && present || tc.wantWire != "" && value != tc.wantWire {
					t.Fatalf("resumed loop effort=%q wire effort=%v present=%v; want canonical=%q wire=%q", gotEffort, value, present, tc.wantEffort, tc.wantWire)
				}
				if body["model"] != profile.Model || body["max_tokens"] != float64(1024) {
					t.Fatalf("runtime model/output settings lost: model=%v max_tokens=%v", body["model"], body["max_tokens"])
				}
			case <-ctx.Done():
				t.Fatal("worker loop sent no physical request")
			}
		})
	}
}
