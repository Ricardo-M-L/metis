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

// Validate the second request on the HTTP wire, not just the Go request value:
// an empty tool result is valid, but omitting function_call_output.output is not.
func TestResponsesToolOutputReplayTwoTurnWire(t *testing.T) {
	result := func(id, text string, isError bool) provider.ContentBlock {
		return provider.ContentBlock{Type: "tool_result", ToolUseID: id, ToolResult: text, IsError: isError}
	}
	for _, profile := range []string{"local", "codex", "compatible", "provider"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct {
				name    string
				results []provider.ContentBlock
			}{
				{"empty", []provider.ContentBlock{result("call_0", "", false)}},
				{"nonempty", []provider.ContentBlock{result("call_0", "saved\n\"中文\"", false)}},
				{"error", []provider.ContentBlock{result("call_0", "permission denied", true)}},
				{"empty_error", []provider.ContentBlock{result("call_0", "", true)}},
				{"mixed", []provider.ContentBlock{result("call_0", "", false), result("call_1", "done", false), result("call_2", "failed", true), result("call_3", "", true)}},
			} {
				t.Run(fmt.Sprintf("%s/stream_%v/%s", profile, streaming, tc.name), func(t *testing.T) {
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
						if request.Stream != (streaming || profile == "codex") {
							t.Errorf("stream = %v", request.Stream)
						}
						wantPrevious := ""
						if profile == "provider" && call == 2 {
							wantPrevious = "resp_tools"
						}
						if request.Store != (profile == "provider") || request.PreviousResponseID != wantPrevious {
							t.Errorf("state changed: store=%v previous=%q", request.Store, request.PreviousResponseID)
						}
						var wantInclude []string
						if profile == "local" || profile == "codex" {
							wantInclude = []string{"reasoning.encrypted_content"}
						}
						if !reflect.DeepEqual(request.Include, wantInclude) {
							t.Errorf("include = %v, want %v", request.Include, wantInclude)
						}
						outputIndex := 0
						for i, item := range request.Input {
							if string(item["type"]) != `"function_call_output"` {
								if _, exists := item["output"]; exists {
									t.Errorf("output leaked to input[%d]: %v", i, item)
								}
								if string(item["type"]) == `"function_call"` && string(item["arguments"]) != `"{}"` {
									t.Errorf("empty tool arguments must replay as {}, got %s", item["arguments"])
								}
								continue
							}
							output, exists := item["output"]
							if !exists {
								w.WriteHeader(http.StatusBadRequest)
								fmt.Fprintf(w, `{"error":{"message":"Missing required parameter: input[%d].output"}}`, i)
								return
							}
							if outputIndex >= len(tc.results) {
								t.Errorf("unexpected extra output: %v", item)
								continue
							}
							want := tc.results[outputIndex]
							var got string
							if err := json.Unmarshal(output, &got); err != nil || string(output) == "null" || got != want.ToolResult {
								t.Errorf("output[%d] = %s, want string %q (err=%v)", outputIndex, output, want.ToolResult, err)
							}
							wantID, _ := json.Marshal(want.ToolUseID)
							if string(item["call_id"]) != string(wantID) {
								t.Errorf("output[%d] call_id = %s, want %s", outputIndex, item["call_id"], wantID)
							}
							outputIndex++
						}
						if call == 2 {
							if outputIndex != len(tc.results) {
								t.Errorf("outputs = %d, want %d", outputIndex, len(tc.results))
							}
							wantCount := 1 + 2*len(tc.results)
							if profile == "provider" {
								wantCount = len(tc.results) // Only results follow the provider checkpoint.
							} else if profile != "compatible" {
								wantCount++ // Preserve the encrypted reasoning item.
							}
							if len(request.Input) != wantCount {
								t.Errorf("replay items = %d, want %d", len(request.Input), wantCount)
							}
						}
						var output []map[string]any
						if call == 1 {
							output = append(output, map[string]any{"id": "rs_tools", "type": "reasoning", "encrypted_content": "test-ciphertext", "summary": []any{}})
							for i, result := range tc.results {
								output = append(output, map[string]any{"id": fmt.Sprintf("fc_%d", i), "type": "function_call", "call_id": result.ToolUseID, "name": "Bash", "arguments": "{}"})
							}
						}
						response := map[string]any{"id": "resp_tools", "status": "completed", "output": output}
						if request.Stream {
							w.Header().Set("Content-Type", "text/event-stream")
							for _, item := range output {
								if item["type"] == "function_call" {
									writeToolOutputReplayEvent(w, "response.output_item.added", "item", item)
								}
								writeToolOutputReplayEvent(w, "response.output_item.done", "item", item)
							}
							writeToolOutputReplayEvent(w, "response.completed", "response", response)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(response)
						}
					}))
					defer server.Close()
					p := NewResponses("test-key", server.URL, "gpt-5.5", 4096, 5*time.Second, 0)
					if err := p.ConfigureCapabilityProfile("openai"); err != nil {
						t.Fatal(err)
					}
					switch profile {
					case "codex":
						p = NewCodexResponses("gpt-5.5", 4096, 5*time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
							return ResponsesOAuthCredential{AccessToken: "test-token", AccountID: "test-account"}, nil
						})
						p.BaseURL = server.URL
					case "compatible":
						if err := p.ConfigureCapabilityProfile("compatible"); err != nil {
							t.Fatal(err)
						}
					case "provider":
						p.StateMode = ResponsesStateProvider
					}
					req := provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "Run the tools."}}}}}
					first, err := toolOutputReplayCall(p, req, streaming)
					if err != nil {
						t.Fatalf("first call: %v", err)
					}
					req.Messages = append(req.Messages,
						provider.Message{Role: provider.RoleAssistant, Content: first},
						provider.Message{Role: provider.RoleUser, Content: tc.results})
					// Restore the entire history, including empty/error tool results and
					// provider checkpoints, across the session persistence boundary.
					persisted, err := json.Marshal(req.Messages)
					if err != nil {
						t.Fatal(err)
					}
					req.Messages = nil
					if err := json.Unmarshal(persisted, &req.Messages); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(req.Messages[len(req.Messages)-1].Content, tc.results) {
						t.Fatal("tool result persistence changed content or error flags")
					}
					if _, err := toolOutputReplayCall(p, req, streaming); err != nil {
						t.Fatalf("second call after tool results: %v", err)
					}
					if calls.Load() != 2 {
						t.Fatalf("HTTP calls = %d, want two", calls.Load())
					}
				})
			}
		}
	}
}

func writeToolOutputReplayEvent(w io.Writer, eventType, field string, value any) {
	data, _ := json.Marshal(map[string]any{"type": eventType, field: value})
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func toolOutputReplayCall(p *Responses, req provider.Request, streaming bool) ([]provider.ContentBlock, error) {
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
		case "provider_state":
			blocks = append(blocks, provider.ContentBlock{Type: event.Type, ProviderHint: event.ProviderHint})
		case "tool_use_start":
			// The fixture's calls have empty object arguments in both parsers.
			blocks = append(blocks, provider.ContentBlock{Type: "tool_use", ToolUseID: event.ToolUseID, ToolName: event.ToolName, ToolInput: map[string]any{}})
		case "error":
			return nil, event.Err
		}
	}
}
