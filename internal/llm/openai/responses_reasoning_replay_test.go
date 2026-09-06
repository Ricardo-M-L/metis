package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// Exercise the actual HTTP encoder and both response parsers across a tool
// round trip. A permissive one-call mock cannot catch a missing replay field.
func TestResponsesReasoningReplayTwoTurnWire(t *testing.T) {
	for _, profile := range []string{"local", "codex"} {
		for _, streaming := range []bool{false, true} {
			for _, summary := range []struct {
				name string
				wire string
				want string
			}{
				{name: "legacy_missing", want: `[]`},
				{name: "empty", wire: `,"summary":[]`, want: `[]`},
				{name: "nonempty", wire: `,"summary":[{"type":"summary_text","text":"Inspect the scene."},{"type":"summary_text","text":"Then plan tests."}]`, want: `[{"type":"summary_text","text":"Inspect the scene."},{"type":"summary_text","text":"Then plan tests."}]`},
			} {
				t.Run(fmt.Sprintf("%s/stream_%v/%s", profile, streaming, summary.name), func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var request struct {
							Stream             bool                         `json:"stream"`
							Input              []map[string]json.RawMessage `json:"input"`
							Store              bool                         `json:"store"`
							Include            []string                     `json:"include"`
							PreviousResponseID string                       `json:"previous_response_id"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Errorf("decode request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						call := calls.Add(1)
						if request.Store || request.PreviousResponseID != "" || !reflect.DeepEqual(request.Include, []string{"reasoning.encrypted_content"}) {
							t.Errorf("local encrypted replay state changed: %+v", request)
						}
						for i, item := range request.Input {
							if string(item["type"]) != `"reasoning"` {
								if _, found := item["summary"]; found {
									t.Errorf("summary leaked to non-reasoning input[%d]: %v", i, item)
								}
								continue
							}
							got := item["summary"]
							if len(got) == 0 {
								w.WriteHeader(http.StatusBadRequest)
								fmt.Fprintf(w, `{"error":{"message":"Missing required parameter: input[%d].summary"}}`, i)
								return
							}
							var actual, expected any
							if err := json.Unmarshal(got, &actual); err != nil {
								t.Errorf("decode summary: %v", err)
							}
							_ = json.Unmarshal([]byte(summary.want), &expected)
							if !reflect.DeepEqual(actual, expected) {
								t.Errorf("summary = %s, want %s", got, summary.want)
							}
							if string(item["id"]) != `"rs_plan"` || string(item["encrypted_content"]) != `"encrypted-plan"` {
								t.Errorf("encrypted reasoning replay changed: %v", item)
							}
						}
						if call == 2 {
							wantTypes := []string{`"message"`, `"reasoning"`, `"function_call"`, `"function_call_output"`}
							var gotTypes []string
							for _, item := range request.Input {
								gotTypes = append(gotTypes, string(item["type"]))
							}
							if !reflect.DeepEqual(gotTypes, wantTypes) {
								t.Errorf("replay input types = %v, want %v", gotTypes, wantTypes)
							}
						}
						if request.Stream {
							w.Header().Set("Content-Type", "text/event-stream")
							if call == 1 {
								fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_plan\",\"type\":\"reasoning\",\"encrypted_content\":\"encrypted-plan\"%s}}\n\n", summary.wire)
								fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_plan\",\"type\":\"function_call\",\"call_id\":\"call_plan\",\"name\":\"TodoWrite\",\"arguments\":\"\"}}\n\n")
								fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_plan\",\"type\":\"function_call\",\"call_id\":\"call_plan\",\"name\":\"TodoWrite\",\"arguments\":\"{}\"}}\n\n")
							}
							fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_plan\",\"status\":\"completed\"}}\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							if call == 1 {
								fmt.Fprintf(w, `{"id":"resp_plan","status":"completed","output":[{"id":"rs_plan","type":"reasoning","encrypted_content":"encrypted-plan"%s},{"id":"fc_plan","type":"function_call","call_id":"call_plan","name":"TodoWrite","arguments":"{}"}]}`, summary.wire)
							} else {
								fmt.Fprint(w, `{"id":"resp_final","status":"completed","output":[]}`)
							}
						}
					}))
					defer server.Close()
					p := NewResponses("test-key", server.URL, "gpt-5.5", 4096, 5*time.Second, 0)
					if err := p.ConfigureCapabilityProfile("openai"); err != nil {
						t.Fatal(err)
					}
					if profile == "codex" {
						p = NewCodexResponses("gpt-5.5", 4096, 5*time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
							return ResponsesOAuthCredential{AccessToken: "test-token", AccountID: "test-account"}, nil
						})
						p.BaseURL = server.URL
					}
					req := provider.Request{SessionID: "same-session", Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "Plan a scene."}}}}}
					first, err := reasoningReplayCall(p, req, streaming)
					if err != nil {
						t.Fatalf("first call: %v", err)
					}
					// The normal session persistence boundary must retain replay metadata.
					persisted, err := json.Marshal(first)
					if err != nil {
						t.Fatal(err)
					}
					var restored []provider.ContentBlock
					if err := json.Unmarshal(persisted, &restored); err != nil {
						t.Fatal(err)
					}
					req.Messages = append(req.Messages,
						provider.Message{Role: provider.RoleAssistant, Content: restored},
						provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: "call_plan", ToolResult: "Plan saved."}}})
					if _, err := reasoningReplayCall(p, req, streaming); err != nil {
						t.Fatalf("second call after TodoWrite: %v", err)
					}
					if calls.Load() != 2 {
						t.Fatalf("HTTP calls = %d, want exactly two", calls.Load())
					}
				})
			}
		}
	}
}

func reasoningReplayCall(p *Responses, req provider.Request, streaming bool) ([]provider.ContentBlock, error) {
	if !streaming {
		response, err := p.Complete(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return response.Content, nil
	}
	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var blocks []provider.ContentBlock
	for {
		event, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return blocks, nil
			}
			return nil, err
		}
		switch event.Type {
		case "redacted_thinking":
			blocks = append(blocks, provider.ContentBlock{Type: event.Type, Data: event.TextDelta, ProviderHint: event.ProviderHint})
		case "tool_use_start":
			blocks = append(blocks, provider.ContentBlock{Type: "tool_use", ToolUseID: event.ToolUseID, ToolName: event.ToolName, ToolInput: map[string]any{}})
		case "error":
			return nil, event.Err
		}
	}
}

func TestResponsesReasoningReplaySummaryOnlyForLocalReasoning(t *testing.T) {
	for _, mode := range []string{"local", "provider", "compatible"} {
		t.Run(mode, func(t *testing.T) {
			p := NewResponses("test-key", "https://api.openai.com/v1", "gpt-5.5", 4096, time.Second, 0)
			if mode == "provider" {
				p.StateMode = ResponsesStateProvider
			} else if mode == "compatible" {
				if err := p.ConfigureCapabilityProfile("compatible"); err != nil {
					t.Fatal(err)
				}
			}
			hints := map[string]string{responsesHintItemID: "rs_old", responsesHintReasoningSummary: `[{"type":"summary_text","text":"Preserved summary."}]`}
			req := provider.Request{Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "start", ProviderHint: hints}}},
				{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
					{Type: "redacted_thinking", Data: "ciphertext", ProviderHint: hints},
					{Type: "text", Text: "Planning.", ProviderHint: hints},
					{Type: "tool_use", ToolUseID: "call_old", ToolName: "TodoWrite", ProviderHint: hints},
					{Type: "provider_state", ProviderHint: map[string]string{responsesHintResponseID: "resp_old", responsesHintStateKey: p.stateKey()}},
				}},
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: "call_old", ToolResult: "saved", ProviderHint: hints}}},
			}}
			body, err := p.buildResponsesRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Input []map[string]json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			reasoningCount := 0
			for _, item := range wire.Input {
				if string(item["type"]) == `"reasoning"` {
					reasoningCount++
					if string(item["summary"]) != hints[responsesHintReasoningSummary] {
						t.Errorf("summary not preserved: %s", item["summary"])
					}
				} else if _, exists := item["summary"]; exists {
					t.Errorf("summary leaked outside reasoning item: %v", item)
				}
			}
			if mode == "local" && reasoningCount != 1 || mode != "local" && reasoningCount != 0 {
				t.Errorf("%s reasoning input count = %d", mode, reasoningCount)
			}
			if mode == "provider" && (body.PreviousResponseID != "resp_old" || !body.Store || len(body.Input) != 1 || body.Input[0].Type != "function_call_output") {
				t.Errorf("provider-managed continuation changed: %+v", body)
			}
		})
	}
}

func TestResponsesReasoningReplayLegacySummaryDefaultsToArray(t *testing.T) {
	for _, hint := range []string{"", "null", "{}", "broken"} {
		t.Run(hint, func(t *testing.T) {
			p := NewResponses("test-key", "https://api.openai.com/v1", "gpt-5.5", 4096, time.Second, 0)
			body, err := p.buildResponsesRequest(provider.Request{Messages: []provider.Message{{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
				Type: "redacted_thinking", Data: "legacy-ciphertext", ProviderHint: map[string]string{responsesHintReasoningSummary: hint},
			}}}}})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Input []struct {
					Summary json.RawMessage `json:"summary"`
				} `json:"input"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire.Input) != 1 || string(wire.Input[0].Summary) != "[]" {
				t.Fatalf("legacy summary must be [], got %s", raw)
			}
		})
	}
}
