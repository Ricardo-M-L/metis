package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestWakeupREPLProcessHelper(t *testing.T) {
	if os.Getenv("METIS_REPL_WAKEUP_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := dispatch(ctx, []string{"chat", "--bare", "--mode", "bypassPermissions"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWakeupREPLProcessIdleProviderNoExtraUser(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requestBody, _ := json.Marshal(request)
		isWake := strings.Contains(string(requestBody), "REPL_IDLE_WAKEUP") && !strings.Contains(string(requestBody), `\"tool_call_id\"`)
		// Tool arguments already contain the marker, so require a standalone
		// synthetic user message rather than the scheduling tool call.
		if messages, ok := request["messages"].([]any); ok {
			isWake = false
			for _, v := range messages {
				m, _ := v.(map[string]any)
				if m["role"] == "user" && strings.Contains(fmt.Sprint(m["content"]), "REPL_IDLE_WAKEUP") {
					isWake = true
				}
			}
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			args := `{"delaySeconds":30,"prompt":"REPL_IDLE_WAKEUP","reason":"authorized idle continuation"}`
			data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_repl", "type": "function", "function": map[string]any{"name": "ScheduleWakeup", "arguments": args}}}}}}})
			fmt.Fprintf(w, "data: %s\n\n", data)
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		text := "waiting idle"
		if isWake {
			text = "REPL_IDLE_SUCCESS"
			defer func() { done <- struct{}{} }()
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q}}]}\n\n", text)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := fmt.Sprintf("[provider]\ndefault=\"repl-fixture\"\n[provider.custom.repl-fixture]\ntransport=\"openai_chat\"\nbase_url=%q\nmodel=\"fixture\"\napi_key_env=\"REPL_FIXTURE_KEY\"\n", server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWakeupREPLProcessHelper$")
	cmd.Env = append(os.Environ(), "METIS_HOME="+home, "METIS_REPL_WAKEUP_HELPER=1", "REPL_FIXTURE_KEY=local-only", "METIS_NO_UPDATE_CHECK=1", "METIS_AUTO_MEMORY=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "repl-output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(stdin, "schedule authorized continuation\n")
	select {
	case <-done:
	case <-ctx.Done():
		_ = cmd.Wait()
		data, _ := os.ReadFile(output.Name())
		t.Fatalf("idle wakeup failed calls=%d output=%s", calls.Load(), data)
	}
	// EOF arrives after provider's third turn; no second user prompt was sent.
	_ = stdin.Close()
	if err = cmd.Wait(); err != nil {
		data, _ := os.ReadFile(output.Name())
		t.Fatalf("REPL failed: %v %s", err, data)
	}
	data, _ := os.ReadFile(output.Name())
	if !strings.Contains(string(data), "status=succeeded") {
		t.Fatalf("missing receipt notification: %s", data)
	}
	svc, err := agent.NewCronService(filepath.Join(home, "sessions", "cron"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := svc.List()
	if len(jobs) != 1 {
		t.Fatalf("jobs=%+v", jobs)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Load(jobs[0].OriginSessionID)
	if err != nil {
		t.Fatal(err)
	}
	users, synthetic := 0, 0
	for _, message := range history {
		isSynthetic, hasText := false, false
		for _, block := range message.Content {
			if block.Type == "text" {
				hasText = true
			}
			if block.Synthetic {
				isSynthetic = true
			}
		}
		if message.Role == "user" && !isSynthetic && hasText {
			users++
		}
		if isSynthetic {
			synthetic++
		}
	}
	if users != 1 || synthetic != 1 {
		t.Fatalf("fabricated user input users=%d synthetic=%d history=%+v", users, synthetic, history)
	}
	records, err := agent.ListCronRuns(svc.Root(), jobs[0].ID, 10)
	if err != nil || len(records) != 1 || records[0].Status != "succeeded" {
		t.Fatalf("receipt=%+v err=%v", records, err)
	}
}
