package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestCLIRunUserInputTraceRecordsRawPrompt(t *testing.T) {
	for _, schemaRetry := range []bool{false, true} {
		t.Run(fmt.Sprintf("schema_retry=%v", schemaRetry), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			for key, value := range map[string]string{
				"METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1", "METIS_NO_UPDATE_CHECK": "1",
				"METIS_RUN_CACHE": "0", "METIS_SIMPLE": "", "METIS_COORDINATOR_MODE": "",
				"METIS_VERIFICATION_URL": "", "METIS_VERIFICATION_TOKEN": "", "METIS_VERIFICATION_REQUIRED_CHECKS": "",
				"METIS_RUN_MAX_SECONDS": "10", "METIS_RECOVERY_MAX_SECONDS": "0",
				"METIS_DUMP_PROMPTS": "0", "METIS_DEBUG": "0", "METIS_TEST_TRACE_KEY": "local-fixture-key",
			} {
				t.Setenv(key, value)
			}
			t.Chdir(t.TempDir())
			if err := os.Mkdir("nested", 0o700); err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string]string{"AGENTS.md": "GENERATED_DIRECTORY_HINT", "file.go": "package fixture"} {
				if err := os.WriteFile(filepath.Join("nested", name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return // Runtime's local connectivity probe is not a model call.
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- string(body)
				answer := "local answer"
				if schemaRetry && len(requests) > 1 {
					answer = `{"ok":true}`
				}
				encoded, err := json.Marshal(answer)
				if err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s,\"role\":\"assistant\"},\"index\":0}]}\n\n", encoded)
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := fmt.Sprintf(`[provider]
default = "trace-fixture"
[provider.custom.trace-fixture]
transport = "openai_chat"
base_url = %q
model = "trace-fixture-model"
api_key_env = "METIS_TEST_TRACE_KEY"
`, server.URL+"/v1")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			previous := rtpkg.CurrentTraceAdapter()
			t.Cleanup(func() {
				if current := rtpkg.CurrentTraceStore(); current != nil {
					_ = current.Close()
				}
				rtpkg.SetTraceAdapter(previous)
				builtin.SetTraceStore(rtpkg.CurrentTraceStore())
				if previous == nil {
					agent.SetTraceHook(nil)
				} else {
					agent.SetTraceHook(previous.OnEvent)
				}
			})
			const sid = "cli-input-trace"
			const prompt = "inspect @nested/file.go"
			args := []string{"--bare", "--no-auth-wizard", "--session-id", sid, "--tools", "Read", "--system", "local fixture"}
			if schemaRetry {
				schemaPath := filepath.Join(t.TempDir(), "schema.json")
				if err := os.WriteFile(schemaPath, []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--output-schema", schemaPath)
			}
			args = append(args, prompt)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := cmdRun(ctx, args); err != nil {
				t.Fatal(err)
			}
			wantRequests := 1
			if schemaRetry {
				wantRequests = 2
			}
			if len(requests) != wantRequests {
				t.Fatalf("local provider requests = %d, want %d", len(requests), wantRequests)
			}
			if request := <-requests; !strings.Contains(request, "GENERATED_DIRECTORY_HINT") {
				t.Fatal("fixture did not exercise provider-only directory hints")
			}
			store := rtpkg.CurrentTraceStore()
			if store == nil {
				t.Fatal("cmdRun did not install trace storage")
			}
			var users []session.TraceEvent
			for _, event := range store.Events(sid) {
				if event.Kind == "user" {
					users = append(users, event)
				}
			}
			if len(users) != 1 || users[0].Text != prompt || users[0].Turn != 1 {
				t.Fatalf("USER trace = %+v, want only original prompt %q in turn 1", users, prompt)
			}
		})
	}
}

func TestCLICronInputTraceRecordsContextOnly(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_AUTO_MEMORY", "0")
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := newCronPermissionRuntime(t, "unused", nil, tools.PermissionAllow, true, t.TempDir())
	rt.loop.Provider.(*cronPermissionProvider).calls = 1 // Return a local final answer without a tool call.
	previous := rtpkg.CurrentTraceAdapter()
	adapter := rtpkg.NewTraceAdapter(store)
	adapter.SetSession(rt.sessionID)
	rtpkg.SetTraceAdapter(adapter)
	agent.SetTraceHook(adapter.OnEvent)
	t.Cleanup(func() {
		rtpkg.SetTraceAdapter(previous)
		if previous == nil {
			agent.SetTraceHook(nil)
		} else {
			agent.SetTraceHook(previous.OnEvent)
		}
		_ = store.Close()
	})
	job := &agent.CronJob{ID: "cron-input-trace", Prompt: "scheduled CLI work", Silent: true}
	if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
		t.Fatal(err)
	}
	var contexts []session.TraceEvent
	for _, event := range store.Events(rt.sessionID) {
		if event.Kind == "user" {
			t.Fatalf("CLI cron created USER trace: %+v", event)
		}
		if event.Kind == "context" && event.Source == "cron" {
			contexts = append(contexts, event)
		}
	}
	if len(contexts) != 1 || contexts[0].Text != job.Prompt || contexts[0].Turn != 1 {
		t.Fatalf("CLI cron context = %+v, want automatic input in turn 1", contexts)
	}
}
