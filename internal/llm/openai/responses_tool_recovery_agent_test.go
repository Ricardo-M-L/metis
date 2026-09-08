package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// This tool has no filesystem or network effects. Its counter proves whether
// the actual Agent dispatch boundary accepted a recovered call.
type recoveryCountingTool struct {
	tools.BaseTool
	mu     sync.Mutex
	values []string
}

func (*recoveryCountingTool) Name() string        { return "RecoveryCount" }
func (*recoveryCountingTool) Description() string { return "Record a value in this test's memory." }
func (*recoveryCountingTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}}
}
func (*recoveryCountingTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (*recoveryCountingTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (c *recoveryCountingTool) Execute(_ context.Context, input map[string]any) (*tools.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, _ := input["value"].(string)
	c.values = append(c.values, value)
	return &tools.Result{Output: "counted:" + value}, nil
}

func TestResponsesToolRecoveryAgentExecutionAndReplay(t *testing.T) {
	const reasoning = `{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_recovery","summary":[],"encrypted_content":"test-ciphertext"}}`
	first := toolRecoveryItem("one", "RecoveryCount", `{"value":"one"}`)
	second := toolRecoveryItem("two", "RecoveryCount", `{"value":"two"}`)
	done := toolRecoveryEvent("done", first)
	for _, tc := range []struct {
		name       string
		events     []string
		wantValues string
		wantCalls  int
		wantStop   string
		wantError  bool
	}{
		{"done_only", []string{reasoning, done, toolRecoveryTerminal("completed", first)}, "one", 1, "end_turn", false},
		{"terminal_only", []string{reasoning, toolRecoveryTerminal("completed", first)}, "one", 1, "end_turn", false},
		{"terminal_multiple_with_prose", []string{reasoning, `{"type":"response.output_text.delta","delta":"Checking."}`, toolRecoveryTerminal("completed", first, second)}, "one,two", 2, "end_turn", false},
		{"duplicate_done_terminal", []string{reasoning, done, done, toolRecoveryTerminal("completed", first), toolRecoveryTerminal("completed", first)}, "one", 1, "end_turn", false},
		{"done_missing_arguments_then_terminal", []string{reasoning, `{"type":"response.output_item.done","item":{"id":"fc_one","call_id":"call_one","type":"function_call","name":"RecoveryCount"}}`, toolRecoveryTerminal("completed", first)}, "one", 1, "end_turn", false},
		{"done_empty_arguments_without_full_snapshot", []string{toolRecoveryEvent("done", toolRecoveryItem("one", "RecoveryCount", "")), toolRecoveryTerminal("completed")}, "", 0, "", true},
		{"added_only_without_final_arguments", []string{toolRecoveryEvent("added", toolRecoveryItem("one", "RecoveryCount", "")), toolRecoveryTerminal("completed")}, "", 0, "", true},
		{"added_partial_arguments_without_final_snapshot", []string{toolRecoveryEvent("added", toolRecoveryItem("one", "RecoveryCount", "")), `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"{\"value\":"}`, toolRecoveryTerminal("completed")}, "", 0, "", true},
		{"added_valid_delta_without_final_snapshot", []string{toolRecoveryEvent("added", toolRecoveryItem("one", "RecoveryCount", "")), `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"{\"value\":\"one\"}"}`, toolRecoveryTerminal("completed")}, "", 0, "", true},
		{"partial_recovery_mixed_with_complete_tool", []string{toolRecoveryTerminal("completed", toolRecoveryItem("one", "RecoveryCount", ""), second)}, "", 0, "", true},
		{"done_then_incomplete", []string{done, toolRecoveryTerminal("incomplete", first)}, "", 0, "provider_incomplete", false},
		{"terminal_incomplete", []string{toolRecoveryTerminal("incomplete", first)}, "", 0, "provider_incomplete", false},
		{"done_then_failed", []string{done, toolRecoveryTerminal("failed", first)}, "", 0, "", true},
		{"done_then_cancelled", []string{done, `{"type":"response.done","response":{"status":"cancelled"}}`}, "", 0, "", true},
		{"done_then_unknown", []string{done, `{"type":"response.done","response":{"status":"unknown"}}`}, "", 0, "", true},
		{"failed_event_with_completed_payload", []string{strings.Replace(toolRecoveryTerminal("completed", first), `"type":"response.completed"`, `"type":"response.failed"`, 1)}, "", 0, "", true},
		{"incomplete_event_with_completed_payload", []string{strings.Replace(toolRecoveryTerminal("completed", first), `"type":"response.completed"`, `"type":"response.incomplete"`, 1)}, "", 0, "", true},
		{"done_then_failed_event_with_completed_payload", []string{done, strings.Replace(toolRecoveryTerminal("completed", first), `"type":"response.completed"`, `"type":"response.failed"`, 1)}, "", 0, "", true},
		{"done_then_incomplete_event_with_completed_payload", []string{done, strings.Replace(toolRecoveryTerminal("completed", first), `"type":"response.completed"`, `"type":"response.incomplete"`, 1)}, "", 0, "", true},
		{"failed_event_without_status", []string{done, `{"type":"response.failed","response":{"output":[]}}`}, "", 0, "", true},
		{"done_then_truncated", []string{done}, "", 0, "", true},
		{"terminal_malformed_arguments", []string{toolRecoveryTerminal("completed", toolRecoveryItem("one", "RecoveryCount", `{"value":`))}, "", 0, "end_turn", false},
		{"done_non_object_arguments", []string{toolRecoveryEvent("done", toolRecoveryItem("one", "RecoveryCount", `["one"]`)), toolRecoveryTerminal("completed")}, "", 0, "end_turn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input []map[string]json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				call := requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if call > 1 && !tc.wantError {
					if tc.wantCalls > 0 {
						var calls, results, encrypted int
						for _, item := range request.Input {
							switch string(item["type"]) {
							case `"function_call"`:
								calls++
								if string(item["call_id"]) != `"call_one"` && string(item["call_id"]) != `"call_two"` {
									t.Errorf("wrong replay call ID: %s", item["call_id"])
								}
							case `"function_call_output"`:
								results++
							case `"reasoning"`:
								encrypted++
								if string(item["summary"]) != "[]" || string(item["encrypted_content"]) != `"test-ciphertext"` {
									t.Errorf("encrypted replay changed: %#v", item)
								}
							}
						}
						if calls != tc.wantCalls || results != tc.wantCalls || encrypted != 1 {
							t.Errorf("replay calls=%d results=%d encrypted=%d, want %d/%d/1", calls, results, encrypted, tc.wantCalls, tc.wantCalls)
						}
					}
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Finished.\"}\n\n")
					fmt.Fprintf(w, "data: %s\n\n", toolRecoveryTerminal("completed"))
					return
				}
				for _, event := range tc.events {
					fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}))
			defer server.Close()
			p := NewResponses("local-test-key", server.URL, "gpt-5.5", 1024, time.Second, 0)
			if err := p.ConfigureCapabilityProfile("openai"); err != nil {
				t.Fatal(err)
			}
			counter := &recoveryCountingTool{}
			registry := tools.NewRegistry()
			registry.Register(counter)
			loop := agent.NewLoop(p, registry, nil, nil, "Local tool recovery test.", 2)
			loop.AppendUser("Record the requested values, then reply.")
			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			defer cancel()
			events := make(chan agent.Event, 128)
			err := loop.Run(ctx, events)
			close(events)
			if (err != nil) != tc.wantError {
				t.Errorf("error = %v, want error=%v", err, tc.wantError)
			}
			var stop string
			for event := range events {
				if event.Kind == agent.EventLoopDone {
					stop = event.StopReason
				}
			}
			counter.mu.Lock()
			values := strings.Join(counter.values, ",")
			executions := len(counter.values)
			counter.mu.Unlock()
			if values != tc.wantValues || executions != tc.wantCalls || stop != tc.wantStop {
				t.Fatalf("executions=%d values=%q stop=%q; want %d/%q/%q", executions, values, stop, tc.wantCalls, tc.wantValues, tc.wantStop)
			}
			if tc.wantCalls > 0 && requests.Load() != 2 {
				t.Errorf("requests = %d, want exactly 2", requests.Load())
			}
		})
	}
}
