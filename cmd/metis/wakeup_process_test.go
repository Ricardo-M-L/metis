package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestWakeupCLIProcessHelper(t *testing.T) {
	if os.Getenv("METIS_WAKEUP_PROCESS_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := dispatch(ctx, []string{"run", "--bare", "--mode", "fullAccess", "schedule the authorized continuation"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Exercise the real CLI in a separate process and the real OpenAI HTTP transport.
// No external credentials/network: the only provider is this loopback fixture.
func TestWakeupCLIProcessToolDueProviderReceipt(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			home := t.TempDir()
			var calls atomic.Int32
			var durableWakeup atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					fmt.Fprint(w, `{"data":[]}`)
					return
				}
				n := calls.Add(1)
				body := map[string]any{}
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 {
					args := `{"delaySeconds":30,"prompt":"WAKEUP_DURABLE_MARKER","reason":"authorized fixture"}`
					delta := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_wakeup", "type": "function", "function": map[string]any{"name": "ScheduleWakeup", "arguments": args}}}}}}}
					data, _ := json.Marshal(delta)
					fmt.Fprintf(w, "data: %s\n\n", data)
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				text := "waiting"
				if n >= 3 {
					store, _ := session.NewStore(filepath.Join(home, "sessions"))
					entries, _ := filepath.Glob(filepath.Join(home, "sessions", "*.jsonl"))
					for _, entry := range entries {
						id := strings.TrimSuffix(filepath.Base(entry), ".jsonl")
						_, history, err := store.Load(id)
						if err == nil {
							for _, message := range history {
								for _, block := range message.Content {
									if block.Synthetic && strings.Contains(block.Text, "<scheduled_wakeup") && strings.Contains(block.Text, "WAKEUP_DURABLE_MARKER") {
										durableWakeup.Store(true)
									}
								}
							}
						}
					}
					if fail {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"message":"fixture denied continuation","type":"invalid_request_error"}}`)
						return
					}
					text = "WAKEUP_SUCCESS"
				}
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q}}]}\n\n", text)
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := fmt.Sprintf("[provider]\ndefault=\"wakeup-fixture\"\n[provider.custom.wakeup-fixture]\ntransport=\"openai_chat\"\nbase_url=%q\nmodel=\"fixture\"\napi_key_env=\"WAKEUP_FIXTURE_KEY\"\n[permissions]\nallow=[\"ScheduleWakeup\"]\n", server.URL+"/v1")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWakeupCLIProcessHelper$")
			cmd.Env = append(os.Environ(), "METIS_HOME="+home, "METIS_WAKEUP_PROCESS_HELPER=1", "WAKEUP_FIXTURE_KEY=local-only", "METIS_NO_UPDATE_CHECK=1", "METIS_RUN_CACHE=0")
			out, err := cmd.CombinedOutput()
			if calls.Load() != 3 {
				t.Fatalf("CLI exited before due continuation: calls=%d err=%v output=%s", calls.Load(), err, out)
			}
			if !durableWakeup.Load() {
				t.Fatalf("provider continuation preceded durable context: %s", out)
			}
			if fail && err == nil {
				t.Fatalf("failed continuation reported success: %s", out)
			}
			if !fail && err != nil {
				t.Fatalf("successful continuation failed: %v %s", err, out)
			}
			svc, loadErr := agent.NewCronService(filepath.Join(home, "sessions", "cron"))
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			jobs := svc.List()
			if len(jobs) != 1 {
				t.Fatalf("jobs=%+v output=%s", jobs, out)
			}
			records, loadErr := agent.ListCronRuns(svc.Root(), jobs[0].ID, 10)
			if loadErr != nil || len(records) != 1 {
				t.Fatalf("receipts=%+v err=%v output=%s", records, loadErr, out)
			}
			want := "succeeded"
			if fail {
				want = "failed"
			}
			if records[0].Status != want || records[0].SessionID == "" {
				t.Fatalf("receipt=%+v", records[0])
			}
			if !strings.Contains(string(out), "[wakeup]") {
				t.Fatalf("missing user notification: %s", out)
			}
		})
	}
}
