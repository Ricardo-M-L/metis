package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelReasoningEffortValuesReachChatAndResponsesWire(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	for _, protocol := range []string{"chat", "responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct {
				name     string
				effort   Effort
				values   map[string]string
				disabled bool
				want     string
				wantErr  bool
			}{
				{name: "mapped", effort: EffortHigh, values: map[string]string{"high": "xhigh"}, want: "xhigh"},
				{name: "default omitted", values: map[string]string{"high": "xhigh"}},
				{name: "disabled suppresses legacy choice", effort: EffortHigh, values: map[string]string{"high": "xhigh"}, disabled: true},
				{name: "unconfigured slot rejected", effort: EffortMedium, values: map[string]string{"high": "xhigh"}, wantErr: true},
				{name: "invalid wire token rejected", effort: EffortHigh, values: map[string]string{"high": "xhigh\nother"}, wantErr: true},
				{name: "legacy mapping retained", effort: EffortHigh, want: "high"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", protocol, streaming, tc.name), func(t *testing.T) {
					var requests atomic.Int32
					bodies := make(chan map[string]any, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("request JSON: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						bodies <- body
						if protocol == "chat" {
							if r.URL.Path != "/chat/completions" {
								t.Errorf("wrong chat route: %s", r.URL.Path)
							}
							if streaming {
								w.Header().Set("Content-Type", "text/event-stream")
								fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
							} else {
								fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
							}
						} else {
							if r.URL.Path != "/responses" {
								t.Errorf("wrong responses route: %s", r.URL.Path)
							}
							response := `{"id":"resp_effort","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`
							if streaming {
								w.Header().Set("Content-Type", "text/event-stream")
								fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
							} else {
								fmt.Fprint(w, response)
							}
						}
					}))
					defer server.Close()
					req := Request{Effort: tc.effort}
					var err error
					if protocol == "chat" {
						p := New("fixture-key", server.URL, "unknown-private-model", 2048, time.Second, 0)
						p.ReasoningEffortValues, p.ReasoningEffortDisabled = tc.values, tc.disabled
						if streaming {
							var stream StreamReader
							stream, err = p.Stream(context.Background(), req)
							if err == nil {
								drainStream(t, stream)
							}
						} else {
							_, err = p.Complete(context.Background(), req)
						}
					} else {
						p := NewResponses("fixture-key", server.URL, "unknown-private-model", 2048, time.Second, 0)
						p.ReasoningEffortValues, p.ReasoningEffortDisabled = tc.values, tc.disabled
						if streaming {
							var stream StreamReader
							stream, err = p.Stream(context.Background(), req)
							if err == nil {
								drainStream(t, stream)
							}
						} else {
							_, err = p.Complete(context.Background(), req)
						}
					}
					if tc.wantErr {
						if err == nil || requests.Load() != 0 {
							t.Fatalf("invalid effort reached wire: error=%v requests=%d", err, requests.Load())
						}
						return
					}
					if err != nil || requests.Load() != 1 {
						t.Fatalf("request failed: error=%v requests=%d", err, requests.Load())
					}
					body := <-bodies
					var value any
					var present bool
					if protocol == "chat" {
						value, present = body["reasoning_effort"]
					} else if reasoning, ok := body["reasoning"].(map[string]any); ok {
						value, present = reasoning["effort"]
					}
					if tc.want == "" && present || tc.want != "" && (!present || value != tc.want) {
						t.Fatalf("wire effort=%v present=%v want=%q", value, present, tc.want)
					}
				})
			}
		}
	}
}

func TestCodexDisabledReasoningPreservesSummaryWithoutEffort(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	p := NewResponses("fixture-key", "https://gateway.example/v1", "model", 2048, time.Second, 0)
	p.ProviderName, p.ReasoningEffortDisabled = "openai-codex", true
	body, err := p.buildResponsesRequest(Request{Effort: EffortHigh})
	if err != nil || body.Reasoning == nil || body.Reasoning.Effort != "" || body.Reasoning.Summary != "auto" {
		t.Fatalf("disabled effort changed independent summary: body=%+v err=%v", body, err)
	}
}
