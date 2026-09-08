package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func toolRecoveryItem(id, name, arguments string) string {
	b, _ := json.Marshal(map[string]string{"type": "function_call", "id": "fc_" + id, "call_id": "call_" + id, "name": name, "arguments": arguments})
	return string(b)
}

func toolRecoveryEvent(kind, item string) string {
	return fmt.Sprintf(`{"type":%q,"item":%s}`, "response.output_item."+kind, item)
}

func toolRecoveryTerminal(status string, items ...string) string {
	return fmt.Sprintf(`{"type":"response.%s","response":{"status":%q,"output":[%s]}}`, status, status, strings.Join(items, ","))
}

func TestResponsesToolRecoveryStreamAndComplete(t *testing.T) {
	first := toolRecoveryItem("one", "Read", `{"path":"one"}`)
	second := toolRecoveryItem("two", "Read", `{"path":"two"}`)
	added := toolRecoveryEvent("added", toolRecoveryItem("one", "Read", ""))
	done := toolRecoveryEvent("done", first)
	terminal := toolRecoveryTerminal("completed", first)
	for _, tc := range []struct {
		name      string
		events    []string
		wantIDs   []string
		wantArgs  []string
		wantText  string
		malformed bool
	}{
		{"done_only", []string{done, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"terminal_only", []string{terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"done_without_terminal_output", []string{done, toolRecoveryTerminal("completed")}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"terminal_with_prose", []string{`{"type":"response.output_text.delta","delta":"Checking now."}`, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "Checking now.", false},
		{"multiple_terminal_tools", []string{toolRecoveryTerminal("completed", first, second)}, []string{"call_one", "call_two"}, []string{`{"path":"one"}`, `{"path":"two"}`}, "", false},
		{"done_reverse_order", []string{toolRecoveryEvent("done", second), done, toolRecoveryTerminal("completed", first, second)}, []string{"call_two", "call_one"}, []string{`{"path":"two"}`, `{"path":"one"}`}, "", false},
		{"normal_stream_no_duplicate", []string{added, `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"{\"path\":"}`, `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"\"one\"}"}`, done, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"interleaved_incremental_tools", []string{added, toolRecoveryEvent("added", toolRecoveryItem("two", "Read", "")), `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"{\"path\":"}`, `{"type":"response.function_call_arguments.delta","item_id":"fc_two","delta":"{\"path\":\"two\"}"}`, `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"\"one\"}"}`, done, toolRecoveryEvent("done", second), toolRecoveryTerminal("completed", first, second)}, []string{"call_one", "call_two"}, []string{`{"path":"one"}`, `{"path":"two"}`}, "", false},
		{"incremental_then_terminal_resync", []string{added, `{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"{\"path\":"}`, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"delta_without_added_then_done", []string{`{"type":"response.function_call_arguments.delta","item_id":"fc_one","delta":"{\"path\":"}`, done, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"duplicate_events", []string{added, added, done, done, terminal, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"done_without_call_id_reuses_added_alias", []string{added, `{"type":"response.output_item.done","item":{"id":"fc_one","type":"function_call","name":"Read","arguments":"{\"path\":\"one\"}"}}`, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"metadata_only_added_then_done", []string{`{"type":"response.output_item.added","item":{"id":"fc_one","type":"function_call"}}`, done, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"metadata_only_added_then_terminal", []string{`{"type":"response.output_item.added","item":{"id":"fc_one","type":"function_call"}}`, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"added_missing_name_retains_call_id", []string{`{"type":"response.output_item.added","item":{"id":"fc_one","type":"function_call","call_id":"call_one"}}`, `{"type":"response.output_item.done","item":{"id":"fc_one","type":"function_call","name":"Read","arguments":"{\"path\":\"one\"}"}}`, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"done_missing_arguments_then_terminal", []string{`{"type":"response.output_item.done","item":{"id":"fc_one","call_id":"call_one","type":"function_call","name":"Read"}}`, terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"done_empty_arguments_then_terminal", []string{toolRecoveryEvent("done", toolRecoveryItem("one", "Read", "")), terminal}, []string{"call_one"}, []string{`{"path":"one"}`}, "", false},
		{"terminal_malformed_arguments", []string{toolRecoveryTerminal("completed", toolRecoveryItem("one", "Read", `{"path":`))}, []string{"call_one"}, []string{`{"path":`}, "", true},
		{"done_non_object_arguments", []string{toolRecoveryEvent("done", toolRecoveryItem("one", "Read", `[1]`)), toolRecoveryTerminal("completed")}, []string{"call_one"}, []string{`[1]`}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, complete := range []bool{false, true} {
				t.Run(fmt.Sprintf("complete_%v", complete), func(t *testing.T) {
					h := newResponsesHarness(t, tc.events)
					p := NewCodexResponses("gpt-5.5", 4096, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
						return ResponsesOAuthCredential{AccessToken: "test-token", AccountID: "test-account"}, nil
					})
					p.BaseURL = h.server.URL
					var ids, args []string
					var text, stop string
					if complete {
						response, err := p.Complete(context.Background(), provider.Request{})
						if err != nil {
							t.Fatal(err)
						}
						stop = response.StopReason
						for _, block := range response.Content {
							if block.Type == "text" {
								text += block.Text
							}
							if block.Type == "tool_use" {
								ids = append(ids, block.ToolUseID)
								if block.ToolName != "Read" || block.ToolInputMalformed != tc.malformed {
									t.Errorf("recovered tool = %#v", block)
								}
								wire, _ := json.Marshal(block.ToolInput)
								args = append(args, string(wire))
							}
						}
					} else {
						stream, err := p.Stream(context.Background(), provider.Request{})
						if err != nil {
							t.Fatal(err)
						}
						started, stopped := map[string]bool{}, map[string]bool{}
						for _, event := range drainStream(t, stream) {
							switch event.Type {
							case "text_delta":
								text += event.TextDelta
							case "tool_use_start":
								if started[event.ToolUseID] || event.ToolName != "Read" {
									t.Errorf("duplicate or invalid start: %#v", event)
								}
								started[event.ToolUseID] = true
								ids = append(ids, event.ToolUseID)
							case "tool_use_stop":
								if !started[event.ToolUseID] || stopped[event.ToolUseID] {
									t.Errorf("stop lacks a unique start: %#v", event)
								}
								stopped[event.ToolUseID] = true
								args = append(args, event.InputDelta)
							case "message_delta":
								stop = event.StopReason
							case "error":
								t.Fatal(event.Err)
							}
						}
					}
					wantArgs := tc.wantArgs
					if complete && tc.malformed {
						wantArgs = []string{"{}"}
					}
					if !reflect.DeepEqual(ids, tc.wantIDs) || !reflect.DeepEqual(args, wantArgs) || text != tc.wantText || stop != "tool_use" {
						t.Fatalf("ids=%v args=%v text=%q stop=%q; want ids=%v args=%v text=%q tool_use", ids, args, text, stop, tc.wantIDs, wantArgs, tc.wantText)
					}
				})
			}
		})
	}
}

func TestResponsesToolRecoveryCompletePreservesNonSuccessBoundary(t *testing.T) {
	item := toolRecoveryItem("one", "Read", `{"path":"one"}`)
	for _, doneFirst := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			terminal string
			wantStop string
		}{
			{"incomplete", toolRecoveryTerminal("incomplete", item), "provider_incomplete"},
			{"failed", toolRecoveryTerminal("failed", item), ""},
			{"failed_missing_status", `{"type":"response.failed","response":{}}`, ""},
			{"cancelled", `{"type":"response.done","response":{"status":"cancelled"}}`, ""},
			{"unknown", `{"type":"response.done","response":{"status":"unknown"}}`, ""},
			{"failed_claims_completed", strings.Replace(toolRecoveryTerminal("completed", item), `"type":"response.completed"`, `"type":"response.failed"`, 1), ""},
			{"incomplete_claims_completed", strings.Replace(toolRecoveryTerminal("completed", item), `"type":"response.completed"`, `"type":"response.incomplete"`, 1), ""},
			{"changed_call_id", toolRecoveryTerminal("completed", strings.Replace(item, `"call_one"`, `"call_other"`, 1)), "tool_use"},
		} {
			t.Run(fmt.Sprintf("done_%v/%s", doneFirst, tc.name), func(t *testing.T) {
				events := []string{tc.terminal}
				if doneFirst {
					events = append([]string{toolRecoveryEvent("done", item)}, events...)
				}
				h := newResponsesHarness(t, events)
				p := NewCodexResponses("gpt-5.5", 4096, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
					return ResponsesOAuthCredential{AccessToken: "test-token", AccountID: "test-account"}, nil
				})
				p.BaseURL = h.server.URL
				response, err := p.Complete(context.Background(), provider.Request{})
				wantStop := tc.wantStop
				if tc.name == "changed_call_id" && doneFirst {
					wantStop = ""
				}
				if wantStop == "" {
					if err == nil || response != nil {
						t.Fatalf("non-success aggregate = %#v, %v; want no response and an error", response, err)
					}
				} else if err != nil || response == nil || response.StopReason != wantStop {
					t.Fatalf("aggregate = %#v, %v; want stop=%q", response, err, wantStop)
				} else if !doneFirst && tc.name == "incomplete" && len(response.Content) != 0 {
					t.Fatalf("incomplete snapshot recovered content: %#v", response.Content)
				}
			})
		}
	}
}

func TestResponsesToolRecoveryRejectsUnsuccessfulTerminalSnapshots(t *testing.T) {
	for _, status := range []string{"incomplete", "failed", "cancelled", "unknown"} {
		t.Run(status, func(t *testing.T) {
			terminal := toolRecoveryTerminal(status, toolRecoveryItem("one", "Read", `{"path":"one"}`))
			if status == "cancelled" || status == "unknown" {
				terminal = strings.Replace(terminal, `"type":"response.`+status+`"`, `"type":"response.done"`, 1)
			}
			h := newResponsesHarness(t, []string{terminal})
			p := newResponsesClient(h)
			stream, err := p.Stream(context.Background(), provider.Request{})
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range drainStream(t, stream) {
				if strings.HasPrefix(event.Type, "tool_") || event.StopReason == "tool_use" {
					t.Fatalf("unsuccessful terminal recovered a call: %#v", event)
				}
			}
		})
	}
}
